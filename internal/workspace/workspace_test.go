package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func TestGitWorktreeIsolationAcrossBranches(t *testing.T) {
	ctx := context.Background()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@soma.local")
	runGit(t, repoDir, "config", "user.name", "soma-test")

	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// original\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	runGit(t, repoDir, "add", "main.go")
	runGit(t, repoDir, "commit", "-m", "initial")

	somaDir := filepath.Join(repoDir, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	run := store.Run{
		ID:            "run_git",
		Title:         "Git worktree test",
		WorkspacePath: repoDir,
		WorkspaceKind: "git",
		GoalSpec:      "isolate branches",
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	b1 := store.Branch{ID: "b1", RunID: run.ID, Label: "branch-1"}
	b2 := store.Branch{ID: "b2", RunID: run.ID, Label: "branch-2"}
	if err := st.CreateBranch(ctx, b1); err != nil {
		t.Fatalf("CreateBranch b1: %v", err)
	}
	if err := st.CreateBranch(ctx, b2); err != nil {
		t.Fatalf("CreateBranch b2: %v", err)
	}

	mgr := NewManager(somaDir)
	ws1, strat1, err := mgr.PrepareBranchWorkspace(ctx, st, run, b1)
	if err != nil {
		t.Fatalf("PrepareBranchWorkspace b1: %v", err)
	}
	ws2, strat2, err := mgr.PrepareBranchWorkspace(ctx, st, run, b2)
	if err != nil {
		t.Fatalf("PrepareBranchWorkspace b2: %v", err)
	}
	if strat1 != StrategyGitWorktree || strat2 != StrategyGitWorktree {
		t.Fatalf("expected git_worktree for both branches, got %s and %s", strat1, strat2)
	}
	if ws1 == ws2 {
		t.Fatalf("expected distinct worktrees, got %s", ws1)
	}

	// Conflicting edits in b1 and b2 must not interfere with each other or root repo.
	if err := os.WriteFile(filepath.Join(ws1, "main.go"), []byte("package main\n// edit from b1\n"), 0o644); err != nil {
		t.Fatalf("write ws1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws2, "main.go"), []byte("package main\n// edit from b2\n"), 0o644); err != nil {
		t.Fatalf("write ws2: %v", err)
	}

	rootBytes, _ := os.ReadFile(filepath.Join(repoDir, "main.go"))
	ws1Bytes, _ := os.ReadFile(filepath.Join(ws1, "main.go"))
	ws2Bytes, _ := os.ReadFile(filepath.Join(ws2, "main.go"))
	if string(rootBytes) != "package main\n// original\n" {
		t.Fatalf("root repo polluted: %q", string(rootBytes))
	}
	if string(ws1Bytes) == string(ws2Bytes) {
		t.Fatalf("branch edits collided")
	}

	// Remote lease mutual exclusion.
	ns1, err := mgr.AcquireRemoteNamespace(ctx, st, "ssh:pchan:gpu0", b1.ID, 60*time.Second)
	if err != nil || ns1 != "runs/run_git/b1" {
		t.Fatalf("AcquireRemoteNamespace b1: ns=%q err=%v", ns1, err)
	}
	if _, err := mgr.AcquireRemoteNamespace(ctx, st, "ssh:pchan:gpu0", b2.ID, 60*time.Second); !errors.Is(err, ErrRemoteResourceBusy) {
		t.Fatalf("expected ErrRemoteResourceBusy for b2, got %v", err)
	}

	b1.WorkspaceDir, b1.WorkspaceStrategy = ws1, strat1
	b2.WorkspaceDir, b2.WorkspaceStrategy = ws2, strat2
	if err := mgr.CleanupBranchWorkspace(ctx, run, b1); err != nil {
		t.Fatalf("cleanup b1: %v", err)
	}
	if err := mgr.CleanupBranchWorkspace(ctx, run, b2); err != nil {
		t.Fatalf("cleanup b2: %v", err)
	}
}

func TestNonGitCopySnapshotIsolation(t *testing.T) {
	ctx := context.Background()
	wsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsRoot, "train.py"), []byte("epochs = 10\n"), 0o644); err != nil {
		t.Fatalf("write train.py: %v", err)
	}

	somaDir := filepath.Join(wsRoot, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	run := store.Run{
		ID:            "run_ml",
		Title:         "Research snapshot",
		WorkspacePath: wsRoot,
		WorkspaceKind: "research",
		GoalSpec:      "ablate lr",
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	b1 := store.Branch{ID: "b_ml1", RunID: run.ID, Label: "lr-1e-4"}
	if err := st.CreateBranch(ctx, b1); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	mgr := NewManager(somaDir)
	ws1, strat1, err := mgr.PrepareBranchWorkspace(ctx, st, run, b1)
	if err != nil {
		t.Fatalf("PrepareBranchWorkspace: %v", err)
	}
	if strat1 != StrategyCopySnapshot {
		t.Fatalf("expected copy_snapshot, got %s", strat1)
	}
	if _, err := os.Stat(filepath.Join(ws1, "artifacts")); err != nil {
		t.Fatalf("expected isolated artifacts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws1, "train.py"), []byte("epochs = 99\n"), 0o644); err != nil {
		t.Fatalf("edit snapshot: %v", err)
	}
	orig, _ := os.ReadFile(filepath.Join(wsRoot, "train.py"))
	if string(orig) != "epochs = 10\n" {
		t.Fatalf("original non-git file modified: %q", string(orig))
	}
}
