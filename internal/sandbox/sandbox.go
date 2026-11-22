package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

var (
	// ErrSandboxUnavailable is returned when FailIfUnavailable is true and no isolation backend can enforce the spec.
	ErrSandboxUnavailable = errors.New("sandbox: no fail-closed OS sandbox backend available")
	// ErrCommandTimeout is returned when a sandboxed command exceeds its configured timeout.
	ErrCommandTimeout = errors.New("sandbox: command timed out")
)

// ExecSpec describes a single command invocation inside the OS sandbox.
type ExecSpec struct {
	WorkspaceDir        string
	PinnedReadOnlyFiles []string
	Command             string
	Args                []string
	ExtraEnv            map[string]string
	AllowNetwork        bool
	Timeout             time.Duration
}

// ExecResult holds captured output, exit status, and backend metadata.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Combined []byte
	Backend  string
	Duration time.Duration
}

// Backend abstracts an OS-level isolation backend (bwrap, Landlock, Seatbelt).
type Backend interface {
	Name() string
	Available() bool
	SupportsNetworkIsolation() bool
	Exec(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// Manager selects and enforces a fail-closed OS sandbox backend.
type Manager struct {
	FailIfUnavailable bool
	Backends          []Backend
}

// NewManager builds a fail-closed sandbox Manager probing host backends in priority order.
func NewManager() *Manager {
	return &Manager{
		FailIfUnavailable: true,
		Backends: []Backend{
			newBwrapBackend(),
			newLandlockBackend(),
			newSeatbeltBackend(),
		},
	}
}

// ActiveBackend returns the first backend able to isolate a default (no-network) spec, if any.
func (m *Manager) ActiveBackend() Backend {
	for _, b := range m.Backends {
		if b != nil && b.Available() && b.SupportsNetworkIsolation() {
			return b
		}
	}
	return nil
}

// Exec runs spec inside the first qualifying OS isolation backend, failing closed when unavailable.
func (m *Manager) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if spec.Command == "" {
		return ExecResult{}, errors.New("sandbox: command is required")
	}
	for _, b := range m.Backends {
		if b == nil || !b.Available() {
			continue
		}
		if !spec.AllowNetwork && !b.SupportsNetworkIsolation() {
			continue
		}
		return b.Exec(ctx, spec)
	}
	if m.FailIfUnavailable {
		return ExecResult{}, ErrSandboxUnavailable
	}
	return ExecResult{}, fmt.Errorf("%w (FailIfUnavailable=false requires explicit unsafe runner)", ErrSandboxUnavailable)
}

// CommandBackend is implemented by backends able to hand back an unstarted, fully sandboxed
// *exec.Cmd, for long-running processes whose lifetime the caller manages (bg_exec).
type CommandBackend interface {
	Command(ctx context.Context, spec ExecSpec) (*exec.Cmd, error)
}

// Command builds an unstarted sandboxed command for spec using the first qualifying backend and
// returns that backend's name. It fails closed: no backend means no command, never a bare host
// process. spec.Timeout is ignored; the caller owns the process lifetime via ctx and Kill.
func (m *Manager) Command(ctx context.Context, spec ExecSpec) (*exec.Cmd, string, error) {
	if spec.Command == "" {
		return nil, "", errors.New("sandbox: command is required")
	}
	for _, b := range m.Backends {
		if b == nil || !b.Available() {
			continue
		}
		if !spec.AllowNetwork && !b.SupportsNetworkIsolation() {
			continue
		}
		cb, ok := b.(CommandBackend)
		if !ok {
			continue
		}
		cmd, err := cb.Command(ctx, spec)
		if err != nil {
			return nil, "", err
		}
		return cmd, b.Name(), nil
	}
	return nil, "", ErrSandboxUnavailable
}

var secretSubstrings = []string{
	"KEY",
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"CREDENTIAL",
	"AUTH",
	"COOKIE",
	"AWS_",
	"GOOGLE_",
	"GCP_",
	"AZURE_",
	"ANTHROPIC_",
	"OPENAI_",
	"GITHUB_",
	"GH_",
	"HF_",
	"RUNPOD_",
}

// IsSecretEnvKey reports whether an environment variable name looks like a credential/secret.
func IsSecretEnvKey(key string) bool {
	up := strings.ToUpper(strings.TrimSpace(key))
	for _, pat := range secretSubstrings {
		if strings.Contains(up, pat) {
			return true
		}
	}
	return false
}

// ScrubEnv constructs a deterministic minimal environment with zero leaked secrets.
func ScrubEnv(extra map[string]string) []string {
	envMap := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/tmp",
		"TMPDIR": "/tmp",
		"LANG":   "C.UTF-8",
	}
	for k, v := range extra {
		if k == "" || IsSecretEnvKey(k) {
			continue
		}
		envMap[k] = v
	}

	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+envMap[k])
	}
	return out
}
