//go:build linux

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

type bwrapBackend struct {
	binPath string
}

func newBwrapBackend() Backend {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return &bwrapBackend{}
	}
	return &bwrapBackend{binPath: path}
}

func (b *bwrapBackend) Name() string { return "bwrap" }

func (b *bwrapBackend) Available() bool { return b.binPath != "" }

func (b *bwrapBackend) SupportsNetworkIsolation() bool { return true }

func (b *bwrapBackend) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if b.binPath == "" {
		return ExecResult{}, ErrSandboxUnavailable
	}
	if spec.WorkspaceDir == "" {
		return ExecResult{}, errors.New("bwrap: WorkspaceDir is required")
	}
	absWS, err := filepath.Abs(spec.WorkspaceDir)
	if err != nil {
		return ExecResult{}, fmt.Errorf("bwrap resolve workspace: %w", err)
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"--ro-bind", "/usr", "/usr",
	}
	// Handle usrmerge symlinks vs real directories dynamically on Linux distros.
	for _, sysPath := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		info, err := os.Lstat(sysPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(sysPath); err == nil {
				args = append(args, "--symlink", target, sysPath)
			}
		} else if info.IsDir() {
			args = append(args, "--ro-bind", sysPath, sysPath)
		}
	}

	args = append(args,
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", absWS, "/workspace",
		"--chdir", "/workspace",
		"--die-with-parent",
		"--clearenv",
	)

	// Enforce kernel-level EROFS read-only binds on pinned oracle files inside /workspace.
	for _, rel := range spec.PinnedReadOnlyFiles {
		cleanRel := filepath.Clean(rel)
		if filepath.IsAbs(cleanRel) || cleanRel == "." || strings.HasPrefix(cleanRel, "..") {
			return ExecResult{}, fmt.Errorf("bwrap: invalid pinned file path %q", rel)
		}
		hostPath := filepath.Join(absWS, cleanRel)
		if _, err := os.Stat(hostPath); err != nil {
			return ExecResult{}, fmt.Errorf("bwrap: pinned file missing %q: %w", rel, err)
		}
		args = append(args, "--ro-bind", hostPath, filepath.ToSlash(filepath.Join("/workspace", cleanRel)))
	}

	if !spec.AllowNetwork {
		args = append(args, "--unshare-net")
	} else {
		args = append(args,
			"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
			"--ro-bind-try", "/etc/ssl", "/etc/ssl",
		)
	}

	for _, kv := range ScrubEnv(spec.ExtraEnv) {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			args = append(args, "--setenv", k, v)
		}
	}

	args = append(args, "--", spec.Command)
	args = append(args, spec.Args...)

	start := time.Now()
	cmd := exec.CommandContext(runCtx, b.binPath, args...)
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
			Backend:  b.Name(),
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
		Backend:  b.Name(),
		Duration: dur,
	}, runErr
}
