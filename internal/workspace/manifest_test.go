package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Files that exist in the project but were never part of the branch workspace (gitignored build
// output, files created while the agent ran) must survive Apply.
func TestApplyKeepsProjectFilesAbsentFromBranch(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	mustWrite(t, filepath.Join(project, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(project, "old.txt"), "remove me\n")

	m := NewManager(filepath.Join(project, ".soma"))
	wsDir, _, err := m.PrepareBranchWorkspace(ctx, nil, store.Run{ID: "r", WorkspacePath: project}, store.Branch{ID: "br1", WorkspaceStrategy: "copy"})
	if err != nil {
		t.Fatal(err)
	}

	// Operator-side files appearing after the snapshot.
	mustWrite(t, filepath.Join(project, ".env"), "SECRET=1\n")
	mustWrite(t, filepath.Join(project, "bin", "tool"), "binary\n")

	// Agent edits one file, deletes one, adds one.
	mustWrite(t, filepath.Join(wsDir, "main.go"), "package main\n\nfunc main() {}\n")
	if err := os.Remove(filepath.Join(wsDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(wsDir, "new.go"), "package main\n")

	if n := m.CountWorkspaceChanges(ctx, project, wsDir); n != 3 {
		t.Fatalf("CountWorkspaceChanges = %d, want 3", n)
	}
	rep, err := m.ApplyBranchWorkspace(ctx, project, wsDir)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !exists(filepath.Join(project, ".env")) || !exists(filepath.Join(project, "bin", "tool")) {
		t.Fatalf("apply deleted project files the branch never had: %+v", rep)
	}
	if exists(filepath.Join(project, "old.txt")) {
		t.Fatal("file deleted by the branch should be deleted in the project")
	}
	if !exists(filepath.Join(project, "new.go")) {
		t.Fatal("file added by the branch missing from the project")
	}
	if n := m.CountWorkspaceChanges(ctx, project, wsDir); n != 0 {
		t.Fatalf("CountWorkspaceChanges after apply = %d, want 0", n)
	}
}

// A file edited in the project while the agent also edited it must not be silently overwritten.
func TestApplyConflictsWhenProjectChangedUnderneath(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	mustWrite(t, filepath.Join(project, "a.txt"), "base\n")
	m := NewManager(filepath.Join(project, ".soma"))
	wsDir, _, err := m.PrepareBranchWorkspace(ctx, nil, store.Run{ID: "r", WorkspacePath: project}, store.Branch{ID: "br1", WorkspaceStrategy: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(project, "a.txt"), "operator edit\n")
	mustWrite(t, filepath.Join(wsDir, "a.txt"), "agent edit\n")

	if _, err := m.ApplyBranchWorkspace(ctx, project, wsDir); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("apply err = %v, want ErrMergeConflict", err)
	}
	got, _ := os.ReadFile(filepath.Join(project, "a.txt"))
	if string(got) != "operator edit\n" {
		t.Fatalf("project file overwritten: %q", got)
	}
}

func TestApplyRefusesToWriteThroughProjectSymlink(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	project := t.TempDir()
	mustWrite(t, filepath.Join(project, "a.txt"), "base\n")
	m := NewManager(filepath.Join(project, ".soma"))
	wsDir, _, err := m.PrepareBranchWorkspace(ctx, nil, store.Run{ID: "r", WorkspacePath: project}, store.Branch{ID: "br1", WorkspaceStrategy: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "data")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(wsDir, "data", "x.txt"), "escape\n")
	if _, err := m.ApplyBranchWorkspace(ctx, project, wsDir); err == nil {
		t.Fatal("expected apply to refuse writing through a symlink")
	}
	if exists(filepath.Join(outside, "x.txt")) {
		t.Fatal("apply wrote outside the project through a symlink")
	}
}
