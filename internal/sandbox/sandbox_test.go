package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScrubEnvAndFailClosedAndSBPL(t *testing.T) {
	env := ScrubEnv(map[string]string{
		"OPENAI_API_KEY":        "sk-123",
		"AWS_SECRET_ACCESS_KEY": "aws-secret",
		"GITHUB_TOKEN":          "ghp_xxx",
		"SOMA_SAFE_VAR":         "hello",
	})
	joined := strings.Join(env, "\n")
	for _, secret := range []string{"sk-123", "aws-secret", "ghp_xxx", "OPENAI_API_KEY", "GITHUB_TOKEN"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("ScrubEnv leaked secret %q in %s", secret, joined)
		}
	}
	if !strings.Contains(joined, "SOMA_SAFE_VAR=hello") {
		t.Fatalf("expected safe extra var preserved, got %s", joined)
	}

	// Fail-closed manager with no backends MUST return ErrSandboxUnavailable.
	closedMgr := &Manager{FailIfUnavailable: true}
	if _, err := closedMgr.Exec(context.Background(), ExecSpec{
		WorkspaceDir: t.TempDir(),
		Command:      "echo",
		Args:         []string{"hi"},
	}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("expected ErrSandboxUnavailable, got %v", err)
	}

	// SBPL determinism check.
	sbpl := GenerateSBPL(ExecSpec{
		WorkspaceDir:        "/tmp/ws",
		PinnedReadOnlyFiles: []string{"tests/pinned_test.go"},
		AllowNetwork:        false,
	})
	if !strings.Contains(sbpl, "(deny network*)") || !strings.Contains(sbpl, "pinned_test.go") {
		t.Fatalf("unexpected SBPL output:\n%s", sbpl)
	}
}

func TestBwrapContainmentAndPinnedReadOnlyOracle(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed on host")
	}

	ctx := context.Background()
	ws := t.TempDir()
	pinnedRel := "pinned_test.go"
	pinnedOrig := "package main\n// IMMUTABLE ASSERTION\n"
	if err := os.WriteFile(filepath.Join(ws, pinnedRel), []byte(pinnedOrig), 0o644); err != nil {
		t.Fatalf("write pinned file: %v", err)
	}

	t.Setenv("OPENAI_API_KEY", "sk-leak-check")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-leak-check")

	mgr := NewManager()

	// 1. Workspace writes succeed inside /workspace.
	resWrite, err := mgr.Exec(ctx, ExecSpec{
		WorkspaceDir: ws,
		Command:      "/bin/sh",
		Args:         []string{"-c", "echo ok > /workspace/work.txt && cat /workspace/work.txt"},
	})
	if err != nil || resWrite.ExitCode != 0 || strings.TrimSpace(string(resWrite.Stdout)) != "ok" {
		t.Fatalf("workspace R/W failed: res=%+v err=%v", resWrite, err)
	}

	// 2. Host SSH/home credentials and env secrets are invisible inside sandbox.
	resExfil, err := mgr.Exec(ctx, ExecSpec{
		WorkspaceDir: ws,
		Command:      "/bin/sh",
		Args:         []string{"-c", "cat ~/.ssh/id_rsa 2>/dev/null || true; cat /home/*/.ssh/id_rsa 2>/dev/null || true; env"},
		ExtraEnv: map[string]string{
			"OPENAI_API_KEY": "sk-extra-leak",
		},
	})
	if err != nil {
		t.Fatalf("exfil probe exec err: %v", err)
	}
	outStr := string(resExfil.Combined)
	for _, needle := range []string{"PRIVATE KEY", "sk-leak-check", "aws-leak-check", "sk-extra-leak", "OPENAI_API_KEY"} {
		if strings.Contains(outStr, needle) {
			t.Fatalf("sandbox leaked %q:\n%s", needle, outStr)
		}
	}

	// 3. --unshare-net blocks network access.
	resNet, err := mgr.Exec(ctx, ExecSpec{
		WorkspaceDir: ws,
		AllowNetwork: false,
		Command:      "/bin/sh",
		Args:         []string{"-c", "(exec 3<>/dev/tcp/1.1.1.1/80) >/dev/null 2>&1 && echo NET_OPEN || echo NET_BLOCKED"},
	})
	if err != nil {
		t.Fatalf("net probe err: %v", err)
	}
	if strings.Contains(string(resNet.Stdout), "NET_OPEN") {
		t.Fatalf("expected network isolated under --unshare-net, got: %s", string(resNet.Combined))
	}

	// 4. Pinned ReadOnly oracle file cannot be overwritten or deleted inside sandbox.
	resTamper, err := mgr.Exec(ctx, ExecSpec{
		WorkspaceDir:        ws,
		PinnedReadOnlyFiles: []string{pinnedRel},
		Command:             "/bin/sh",
		Args:                []string{"-c", "echo hacked > /workspace/pinned_test.go || rm -f /workspace/pinned_test.go"},
	})
	if err != nil {
		t.Fatalf("tamper test exec err: %v", err)
	}
	if resTamper.ExitCode == 0 {
		t.Fatalf("expected non-zero exit when modifying pinned read-only file, got 0")
	}
	gotPinned, err := os.ReadFile(filepath.Join(ws, pinnedRel))
	if err != nil || string(gotPinned) != pinnedOrig {
		t.Fatalf("pinned file corrupted! got %q err=%v", string(gotPinned), err)
	}
}
