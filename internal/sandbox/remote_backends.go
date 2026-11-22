package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DockerBackend executes commands inside an ephemeral Docker container with isolated network
// (--network=none when AllowNetwork=false), read-only pinned oracle bind mounts, and scrubbed env.
type DockerBackend struct {
	BinPath string
	Image   string
}

// NewDockerBackend constructs a Docker container isolation backend using image (default "alpine:3.20").
func NewDockerBackend(image string) *DockerBackend {
	path, _ := exec.LookPath("docker")
	if strings.TrimSpace(image) == "" {
		image = "alpine:3.20"
	}
	return &DockerBackend{
		BinPath: path,
		Image:   image,
	}
}

func (d *DockerBackend) Name() string { return "docker" }

func (d *DockerBackend) Available() bool { return d.BinPath != "" && d.Image != "" }

func (d *DockerBackend) SupportsNetworkIsolation() bool { return true }

// BuildArgs constructs the deterministic `docker run` CLI arguments for spec (exported for verification tests).
func (d *DockerBackend) BuildArgs(absWS string, spec ExecSpec) ([]string, error) {
	args := []string{
		"run", "--rm", "--init",
		"-v", fmt.Sprintf("%s:/workspace", absWS),
		"-w", "/workspace",
	}
	if !spec.AllowNetwork {
		args = append(args, "--network=none")
	}
	for _, rel := range spec.PinnedReadOnlyFiles {
		cleanRel := filepath.Clean(rel)
		if filepath.IsAbs(cleanRel) || cleanRel == "." || strings.HasPrefix(cleanRel, "..") {
			return nil, fmt.Errorf("docker: invalid pinned file path %q", rel)
		}
		hostPath := filepath.Join(absWS, cleanRel)
		if _, err := os.Stat(hostPath); err != nil {
			return nil, fmt.Errorf("docker: pinned file missing %q: %w", rel, err)
		}
		containerPath := filepath.ToSlash(filepath.Join("/workspace", cleanRel))
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", hostPath, containerPath))
	}
	for _, kv := range ScrubEnv(spec.ExtraEnv) {
		args = append(args, "-e", kv)
	}
	args = append(args, d.Image, spec.Command)
	args = append(args, spec.Args...)
	return args, nil
}

func (d *DockerBackend) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if !d.Available() {
		return ExecResult{}, ErrSandboxUnavailable
	}
	if spec.WorkspaceDir == "" {
		return ExecResult{}, errors.New("docker: WorkspaceDir is required")
	}
	absWS, err := filepath.Abs(spec.WorkspaceDir)
	if err != nil {
		return ExecResult{}, err
	}
	args, err := d.BuildArgs(absWS, spec)
	if err != nil {
		return ExecResult{}, err
	}
	return runExternalBackendCmd(ctx, d.Name(), d.BinPath, args, spec.Timeout)
}

// SSHBackend executes commands on a remote compute host (e.g., pchan GPU node or lila VM lab)
// over non-interactive SSH inside an isolated remote workspace directory with scrubbed env exports.
type SSHBackend struct {
	BinPath    string
	TargetHost string
	RemoteRoot string
	SSHOptions []string
}

// NewSSHBackend constructs a remote SSH execution backend targeting targetHost under remoteRoot.
func NewSSHBackend(targetHost, remoteRoot string, sshOpts ...string) *SSHBackend {
	path, _ := exec.LookPath("ssh")
	if strings.TrimSpace(remoteRoot) == "" {
		remoteRoot = "/tmp/soma-remote"
	}
	return &SSHBackend{
		BinPath:    path,
		TargetHost: strings.TrimSpace(targetHost),
		RemoteRoot: strings.TrimSpace(remoteRoot),
		SSHOptions: sshOpts,
	}
}

func (s *SSHBackend) Name() string { return "ssh:" + s.TargetHost }

func (s *SSHBackend) Available() bool { return s.BinPath != "" && s.TargetHost != "" }

func (s *SSHBackend) SupportsNetworkIsolation() bool { return true }

// BuildArgs constructs the deterministic `ssh` CLI invocation for spec (exported for verification tests).
func (s *SSHBackend) BuildArgs(spec ExecSpec) []string {
	wsBase := "default"
	if spec.WorkspaceDir != "" {
		wsBase = filepath.Base( filepath.Clean(spec.WorkspaceDir))
	}
	remoteDir := filepath.ToSlash(filepath.Join(s.RemoteRoot, wsBase))

	var envExports []string
	for _, kv := range ScrubEnv(spec.ExtraEnv) {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			envExports = append(envExports, fmt.Sprintf("%s=%s", k, shellQuote(v)))
		}
	}

	var cmdParts []string
	cmdParts = append(cmdParts, shellQuote(spec.Command))
	for _, a := range spec.Args {
		cmdParts = append(cmdParts, shellQuote(a))
	}

	remoteScript := fmt.Sprintf("mkdir -p %s && cd %s && env -i %s %s",
		shellQuote(remoteDir),
		shellQuote(remoteDir),
		strings.Join(envExports, " "),
		strings.Join(cmdParts, " "),
	)

	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
	}
	args = append(args, s.SSHOptions...)
	args = append(args, s.TargetHost, remoteScript)
	return args
}

func (s *SSHBackend) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if !s.Available() {
		return ExecResult{}, ErrSandboxUnavailable
	}
	args := s.BuildArgs(spec)
	return runExternalBackendCmd(ctx, s.Name(), s.BinPath, args, spec.Timeout)
}

func runExternalBackendCmd(ctx context.Context, backendName, binPath string, args []string, timeout time.Duration) (ExecResult, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(runCtx, binPath, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	dur := time.Since(start)

	combined := append([]byte(nil), stdout.Bytes()...)
	combined = append(combined, stderr.Bytes()...)

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return ExecResult{
			ExitCode: -1,
			Stdout:   stdout.Bytes(),
			Stderr:   stderr.Bytes(),
			Combined: combined,
			Backend:  backendName,
			Duration: dur,
		}, ErrCommandTimeout
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
			runErr = nil
		}
	}
	return ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Combined: combined,
		Backend:  backendName,
		Duration: dur,
	}, runErr
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
