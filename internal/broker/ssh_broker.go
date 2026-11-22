package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// HostPolicy defines an allowlisted SSH target and command prefix constraints.
type HostPolicy struct {
	Alias                  string
	Hostname               string
	User                   string
	AllowedCommandPrefixes []string
	ResourceKey            string
	LeaseTTL               time.Duration
}

// SSHCommandRunner executes a validated SSH invocation on the host.
type SSHCommandRunner interface {
	RunSSH(ctx context.Context, host HostPolicy, remoteNamespace, cmdStr string) (stdout []byte, stderr []byte, exitCode int, err error)
}

type execSSHRunner struct{}

func (r *execSSHRunner) RunSSH(ctx context.Context, host HostPolicy, remoteNamespace, cmdStr string) ([]byte, []byte, int, error) {
	target := host.Hostname
	if target == "" {
		target = host.Alias
	}
	if host.User != "" {
		target = host.User + "@" + target
	}
	remoteCmd := cmdStr
	if remoteNamespace != "" {
		remoteCmd = fmt.Sprintf("SOMA_REMOTE_NS=%s %s", shellQuoteSingle(remoteNamespace), cmdStr)
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", target, remoteCmd)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			err = nil
		}
	}
	return outBuf.Bytes(), errBuf.Bytes(), exitCode, err
}

// SSHBroker executes prefix-allowlisted remote commands without exposing ~/.ssh to the sandbox.
type SSHBroker struct {
	Store    *store.Store
	WS       *workspace.Manager
	Policies map[string]HostPolicy
	Runner   SSHCommandRunner
}

// NewSSHBroker builds an SSHBroker backed by st and ws.
func NewSSHBroker(st *store.Store, ws *workspace.Manager, runner SSHCommandRunner, policies ...HostPolicy) *SSHBroker {
	if runner == nil {
		runner = &execSSHRunner{}
	}
	pm := make(map[string]HostPolicy, len(policies))
	for _, p := range policies {
		pm[p.Alias] = p
	}
	return &SSHBroker{
		Store:    st,
		WS:       ws,
		Policies: pm,
		Runner:   runner,
	}
}

func (s *SSHBroker) Name() string { return "ssh" }

var forbiddenShellMeta = []string{";", "&&", "||", "|", "`", "$(", "\n", "\r", ">", "<"}

// Validate enforces host allowlisting, shell metacharacter rejection, and command prefix allowlists.
func (s *SSHBroker) Validate(req BrokerRequest) error {
	_, _, err := s.validateAndParse(req)
	return err
}

func (s *SSHBroker) validateAndParse(req BrokerRequest) (HostPolicy, string, error) {
	pol, ok := s.Policies[req.Target]
	if !ok {
		return HostPolicy{}, "", fmt.Errorf("%w: ssh host alias %q not allowlisted", ErrPolicyDenied, req.Target)
	}

	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(req.ParamsJSON), &params); err != nil {
		return HostPolicy{}, "", fmt.Errorf("ssh broker parse params: %w", err)
	}
	cmdStr := strings.TrimSpace(params.Command)
	if cmdStr == "" {
		return HostPolicy{}, "", fmt.Errorf("%w: empty ssh command", ErrPolicyDenied)
	}
	for _, meta := range forbiddenShellMeta {
		if strings.Contains(cmdStr, meta) {
			return HostPolicy{}, "", fmt.Errorf("%w: disallowed shell metacharacter %q in ssh command", ErrPolicyDenied, meta)
		}
	}
	if !matchesAnyPrefix(cmdStr, pol.AllowedCommandPrefixes) {
		return HostPolicy{}, "", fmt.Errorf("%w: ssh command %q not allowed on %s", ErrPolicyDenied, cmdStr, pol.Alias)
	}
	return pol, cmdStr, nil
}

func (s *SSHBroker) Execute(ctx context.Context, req BrokerRequest) (BrokerResponse, error) {
	pol, cmdStr, err := s.validateAndParse(req)
	if err != nil {
		return BrokerResponse{}, err
	}

	var remoteNS string
	if pol.ResourceKey != "" && s.WS != nil && s.Store != nil && req.BranchID != "" {
		ttl := pol.LeaseTTL
		if ttl <= 0 {
			ttl = 60 * time.Second
		}
		ns, err := s.WS.AcquireRemoteNamespace(ctx, s.Store, pol.ResourceKey, req.BranchID, ttl)
		if err != nil {
			return BrokerResponse{}, err
		}
		remoteNS = ns
		defer func() {
			_ = s.Store.ReleaseRemoteLease(context.Background(), pol.ResourceKey, req.BranchID)
		}()
	}

	stdout, stderr, exitCode, err := s.Runner.RunSSH(ctx, pol, remoteNS, cmdStr)
	if err != nil {
		return BrokerResponse{}, err
	}
	combined := append([]byte(nil), stdout...)
	combined = append(combined, stderr...)
	status := 200
	if exitCode != 0 {
		status = 500
	}
	return BrokerResponse{
		StatusCode:  status,
		Body:        combined,
		ContentType: "text/plain",
		TrustTier:   "untrusted_external",
	}, nil
}

func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
