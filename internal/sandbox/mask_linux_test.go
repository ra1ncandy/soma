//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// In direct mode the workspace is the project directory: kernel state under .soma must be
// invisible to sandboxed commands, and git hooks/config must not be writable.
func TestBwrapMasksKernelStateAndGitHooks(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}
	ws := t.TempDir()
	for _, d := range []string{".soma", ".git/hooks"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, ".soma", "harness.json"), []byte("sk-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager()
	res, err := mgr.Exec(context.Background(), ExecSpec{
		WorkspaceDir: ws,
		Command:      "/bin/sh",
		Args: []string{"-c", "cat .soma/harness.json; echo x > .soma/planted; " +
			"echo '#!/bin/sh' > .git/hooks/pre-commit; echo 'evil' >> .git/config; echo done"},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Skipf("sandbox unavailable in this environment: %v", err)
	}
	if strings.Contains(string(res.Combined), "sk-secret") {
		t.Fatalf("sandbox could read .soma: %s", res.Combined)
	}
	if _, err := os.Stat(filepath.Join(ws, ".soma", "planted")); err == nil {
		t.Fatal("sandbox wrote into the host .soma directory")
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatal("sandbox created a git hook on the host")
	}
	if cfg, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); strings.Contains(string(cfg), "evil") {
		t.Fatal("sandbox modified .git/config on the host")
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	c := &cappedBuffer{limit: 8}
	_, _ = c.Write([]byte("12345"))
	_, _ = c.Write([]byte("67890"))
	out := string(c.Bytes())
	if !strings.HasPrefix(out, "12345678") || !strings.Contains(out, "truncated") {
		t.Fatalf("unexpected capped output %q", out)
	}
}
