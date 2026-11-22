package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type seatbeltBackend struct {
	binPath string
}

func newSeatbeltBackend() Backend {
	if runtime.GOOS != "darwin" {
		return &seatbeltBackend{}
	}
	p, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return &seatbeltBackend{}
	}
	return &seatbeltBackend{binPath: p}
}

func (s *seatbeltBackend) Name() string                   { return "seatbelt" }
func (s *seatbeltBackend) Available() bool                { return s.binPath != "" }
func (s *seatbeltBackend) SupportsNetworkIsolation() bool { return true }

// GenerateSBPL produces a deterministic Apple Seatbelt profile denying credential directories,
// confining writes to WorkspaceDir + /tmp, and denying network sockets when AllowNetwork is false.
func GenerateSBPL(spec ExecSpec) string {
	ws := spec.WorkspaceDir
	if ws != "" {
		if abs, err := filepath.Abs(ws); err == nil {
			ws = abs
		}
	}
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-read* (subpath \"/Users\"))\n")
	b.WriteString("(deny file-read* (regex #\"^.*/\\.ssh(/.*)?$\"))\n")
	b.WriteString("(deny file-read* (regex #\"^.*/\\.aws(/.*)?$\"))\n")
	b.WriteString("(deny file-read* (regex #\"^.*/\\.gnupg(/.*)?$\"))\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	if ws != "" {
		fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", ws)
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", ws)
	}
	pinned := append([]string(nil), spec.PinnedReadOnlyFiles...)
	sort.Strings(pinned)
	for _, rel := range pinned {
		if ws != "" {
			fmt.Fprintf(&b, "(deny file-write* (literal %q))\n", filepath.Join(ws, rel))
		}
	}
	if !spec.AllowNetwork {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

func (s *seatbeltBackend) Exec(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	if s.binPath == "" {
		return ExecResult{}, ErrSandboxUnavailable
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	profile := GenerateSBPL(spec)
	args := append([]string{"-p", profile, spec.Command}, spec.Args...)
	cmd := exec.CommandContext(runCtx, s.binPath, args...)
	cmd.Dir = spec.WorkspaceDir
	cmd.Env = ScrubEnv(spec.ExtraEnv)

	start := time.Now()
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
			Backend:  s.Name(),
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
		Backend:  s.Name(),
		Duration: dur,
	}, runErr
}
