package dag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func newFSToolRunner(t *testing.T) *LocalToolRunner {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewLocalToolRunner(vmem.NewManager(st))
}

// The model can create symlinks inside the workspace via exec; host-side fs tools must not follow
// them out of the workspace.
func TestFSToolsDoNotFollowSymlinksOutOfWorkspace(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("HOST-SECRET needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "filelink")); err != nil {
		t.Fatal(err)
	}
	r := newFSToolRunner(t)

	for _, p := range []string{"dirlink/secret.txt", "filelink"} {
		out, err := r.Execute(ctx, ToolCall{Name: "fs_read", ArgsJSON: `{"path":"` + p + `"}`, WorkspaceDir: ws})
		if err == nil || strings.Contains(string(out.Content), "HOST-SECRET") {
			t.Fatalf("fs_read %s escaped the workspace: content=%q err=%v", p, out.Content, err)
		}
	}
	if _, err := r.Execute(ctx, ToolCall{Name: "fs_write", ArgsJSON: `{"path":"dirlink/planted.txt","content":"x"}`, WorkspaceDir: ws}); err == nil {
		t.Fatal("fs_write through a symlinked directory should fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("fs_write created a file outside the workspace")
	}
	if _, err := r.Execute(ctx, ToolCall{Name: "fs_write", ArgsJSON: `{"path":"filelink","content":"overwritten"}`, WorkspaceDir: ws}); err == nil {
		t.Fatal("fs_write through a symlinked file should fail")
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); !strings.Contains(string(got), "HOST-SECRET") {
		t.Fatal("fs_write overwrote a file outside the workspace")
	}
	out, err := r.Execute(ctx, ToolCall{Name: "fs_grep", ArgsJSON: `{"pattern":"needle"}`, WorkspaceDir: ws})
	if err != nil || strings.Contains(string(out.Content), "HOST-SECRET") {
		t.Fatalf("fs_grep read through a symlink: content=%q err=%v", out.Content, err)
	}
}

func TestFSToolsProtectKernelAndGitDirs(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".soma"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".soma", "harness.json"), []byte(`{"api_key":"sk-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newFSToolRunner(t)
	if out, err := r.Execute(ctx, ToolCall{Name: "fs_read", ArgsJSON: `{"path":".soma/harness.json"}`, WorkspaceDir: ws}); err == nil {
		t.Fatalf("fs_read of .soma should be refused, got %q", out.Content)
	}
	if _, err := r.Execute(ctx, ToolCall{Name: "fs_write", ArgsJSON: `{"path":".git/hooks/pre-commit","content":"#!/bin/sh"}`, WorkspaceDir: ws}); err == nil {
		t.Fatal("fs_write into .git should be refused")
	}
}

func TestFSWriteReadPatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	r := newFSToolRunner(t)
	if _, err := r.Execute(ctx, ToolCall{Name: "fs_write", ArgsJSON: `{"path":"a/b/c.txt","content":"one\ntwo\nthree\n"}`, WorkspaceDir: ws}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(ctx, ToolCall{Name: "fs_patch", ArgsJSON: `{"path":"a/b/c.txt","search":"two","replace":"TWO"}`, WorkspaceDir: ws}); err != nil {
		t.Fatal(err)
	}
	out, err := r.Execute(ctx, ToolCall{Name: "fs_read", ArgsJSON: `{"path":"a/b/c.txt","start_line":2,"end_line":2}`, WorkspaceDir: ws})
	if err != nil || !strings.Contains(string(out.Content), "L2: TWO") {
		t.Fatalf("round trip: %q err=%v", out.Content, err)
	}
	out, err = r.Execute(ctx, ToolCall{Name: "fs_grep", ArgsJSON: `{"pattern":"T[A-Z]O","path":"a"}`, WorkspaceDir: ws})
	if err != nil || !strings.Contains(string(out.Content), "a/b/c.txt:L2: TWO") {
		t.Fatalf("grep: %q err=%v", out.Content, err)
	}
	out, err = r.Execute(ctx, ToolCall{Name: "fs_list", ArgsJSON: `{}`, WorkspaceDir: ws})
	if err != nil || strings.TrimSpace(string(out.Content)) != "a/b/c.txt" {
		t.Fatalf("list: %q err=%v", out.Content, err)
	}
}
