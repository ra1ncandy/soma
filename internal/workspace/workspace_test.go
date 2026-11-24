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
	ns1, err := mgr.AcquireRemoteNamespace(ctx, st, "ssh:gpu-node-01:gpu0", b1.ID, 60*time.Second)
	if err != nil || ns1 != "runs/run_git/b1" {
		t.Fatalf("AcquireRemoteNamespace b1: ns=%q err=%v", ns1, err)
	}
	if _, err := mgr.AcquireRemoteNamespace(ctx, st, "ssh:gpu-node-01:gpu0", b2.ID, 60*time.Second); !errors.Is(err, ErrRemoteResourceBusy) {
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

func TestDirectStrategyAndDiffApply(t *testing.T) {
	ctx := context.Background()
	wsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsRoot, "app.txt"), []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "obsolete.txt"), []byte("remove me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	somaDir := filepath.Join(wsRoot, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	run := store.Run{
		ID:            "run_direct",
		Title:         "Direct & Diff test",
		WorkspacePath: wsRoot,
		WorkspaceKind: "research",
		GoalSpec:      "test direct + diff",
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	// 1. Direct strategy on root branch points straight at wsRoot and is never deleted by Cleanup.
	rootBr := store.Branch{ID: "b_direct", RunID: run.ID, Label: "main", WorkspaceStrategy: StrategyDirect}
	if err := st.CreateBranch(ctx, rootBr); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(somaDir)
	dirDirect, stratDirect, err := mgr.PrepareBranchWorkspace(ctx, st, run, rootBr)
	if err != nil {
		t.Fatalf("PrepareBranchWorkspace direct: %v", err)
	}
	if stratDirect != StrategyDirect || dirDirect != wsRoot {
		t.Fatalf("expected direct strategy at %s, got strat=%s dir=%s", wsRoot, stratDirect, dirDirect)
	}
	rootBr.WorkspaceDir, rootBr.WorkspaceStrategy = dirDirect, stratDirect
	if err := mgr.CleanupBranchWorkspace(ctx, run, rootBr); err != nil {
		t.Fatalf("CleanupBranchWorkspace direct: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "app.txt")); err != nil {
		t.Fatalf("CleanupBranchWorkspace deleted direct root file: %v", err)
	}

	// 2. Forked branch requesting direct falls back to isolated snapshot so forks never clobber root.
	forkBr := store.Branch{ID: "b_fork", RunID: run.ID, ParentBranchID: rootBr.ID, Label: "fork", WorkspaceStrategy: StrategyDirect}
	if err := st.CreateBranch(ctx, forkBr); err != nil {
		t.Fatal(err)
	}
	forkDir, forkStrat, err := mgr.PrepareBranchWorkspace(ctx, st, run, forkBr)
	if err != nil {
		t.Fatalf("PrepareBranchWorkspace fork: %v", err)
	}
	if forkStrat != StrategyCopySnapshot || forkDir == wsRoot {
		t.Fatalf("expected forked branch to isolate as copy_snapshot, got strat=%s dir=%s", forkStrat, forkDir)
	}

	// Mutate fork workspace: modify app.txt, add created.txt, delete obsolete.txt.
	if err := os.WriteFile(filepath.Join(forkDir, "app.txt"), []byte("line 1\nline 2 updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(forkDir, "created.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(forkDir, "obsolete.txt")); err != nil {
		t.Fatal(err)
	}

	if cnt := mgr.CountWorkspaceChanges(ctx, wsRoot, forkDir); cnt != 3 {
		t.Fatalf("expected 3 workspace changes, got %d", cnt)
	}
	rep, err := mgr.DiffBranchWorkspace(ctx, wsRoot, forkDir)
	if err != nil {
		t.Fatalf("DiffBranchWorkspace: %v", err)
	}
	if rep.TotalChanges != 3 || len(rep.Files) != 3 {
		t.Fatalf("unexpected diff report: %+v", rep)
	}

	applied, err := mgr.ApplyBranchWorkspace(ctx, wsRoot, forkDir)
	if err != nil {
		t.Fatalf("ApplyBranchWorkspace: %v", err)
	}
	if len(applied.AddedFiles)+len(applied.ModifiedFiles)+len(applied.DeletedFiles) != 3 {
		t.Fatalf("expected 3 applied paths, got %+v", applied)
	}
	if got, _ := os.ReadFile(filepath.Join(wsRoot, "app.txt")); string(got) != "line 1\nline 2 updated\n" {
		t.Fatalf("app.txt not updated: %q", string(got))
	}
	if got, _ := os.ReadFile(filepath.Join(wsRoot, "created.txt")); string(got) != "brand new\n" {
		t.Fatalf("created.txt not copied: %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected obsolete.txt deleted after apply, err=%v", err)
	}
	if cnt := mgr.CountWorkspaceChanges(ctx, wsRoot, forkDir); cnt != 0 {
		t.Fatalf("expected 0 changes remaining after apply, got %d", cnt)
	}
}
