package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeBranchWorkspaces_DisjointThreeWayAndConflict(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mgr := NewManager(filepath.Join(tmp, ".soma"))

	baseDir := filepath.Join(tmp, "base")
	workerA := filepath.Join(tmp, "workerA")
	workerB := filepath.Join(tmp, "workerB")
	targetDir := filepath.Join(tmp, "target")

	sharedBase := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	for _, d := range []string{baseDir, workerA, workerB, targetDir} {
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "shared.txt"), []byte(sharedBase), 0o644)
	}

	// Worker A adds backend.go and edits top of shared.txt (line1 -> line1_A).
	_ = os.WriteFile(filepath.Join(workerA, "backend.go"), []byte("package app\nfunc Backend() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workerA, "shared.txt"),
		[]byte("line1_A\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"), 0o644)

	// Worker B adds frontend.go and edits bottom of shared.txt (line8 -> line8_B).
	_ = os.WriteFile(filepath.Join(workerB, "frontend.go"), []byte("package app\nfunc Frontend() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(workerB, "shared.txt"),
		[]byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8_B\n"), 0o644)

	rep, err := mgr.MergeBranchWorkspaces(ctx, baseDir, []string{workerA, workerB}, targetDir)
	if err != nil {
		t.Fatalf("MergeBranchWorkspaces clean: %v", err)
	}
	if len(rep.AddedFiles) != 2 || len(rep.ThreeWayMerged) != 1 || rep.ThreeWayMerged[0] != "shared.txt" {
		t.Fatalf("unexpected merge report: %+v", rep)
	}

	mergedShared, err := os.ReadFile(filepath.Join(targetDir, "shared.txt"))
	if err != nil {
		t.Fatalf("ReadFile shared.txt: %v", err)
	}
	if !strings.Contains(string(mergedShared), "line1_A") || !strings.Contains(string(mergedShared), "line8_B") {
		t.Fatalf("expected both 3-way hunks in shared.txt, got:\n%s", string(mergedShared))
	}

	// Now create Worker C that edits line1 differently (line1 -> line1_CONFLICT) and verify fail-closed conflict.
	workerC := filepath.Join(tmp, "workerC")
	_ = os.MkdirAll(workerC, 0o755)
	_ = os.WriteFile(filepath.Join(workerC, "shared.txt"),
		[]byte("line1_CONFLICT\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"), 0o644)

	_, err = mgr.MergeBranchWorkspaces(ctx, baseDir, []string{workerA, workerC}, filepath.Join(tmp, "target2"))
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict on overlapping line1 edit, got %v", err)
	}
}

func TestMergeBranchWorkspaces_RealGitWorktreesAndPycache(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@soma.local")
	runGit("config", "user.name", "Soma Test")

	baseShared := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte(baseShared), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "app", "__init__.py"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")

	wtA := filepath.Join(tmp, "wtA")
	wtB := filepath.Join(tmp, "wtB")
	runGit("worktree", "add", "--detach", wtA, "HEAD")
	runGit("worktree", "add", "--detach", wtB, "HEAD")

	// Confirm real git worktree checkouts use a regular file named .git.
	if info, err := os.Stat(filepath.Join(wtA, ".git")); err != nil || info.IsDir() {
		t.Fatalf("expected .git pointer file in worktree, got info=%v err=%v", info, err)
	}

	if err := os.WriteFile(filepath.Join(wtA, "shared.txt"),
		[]byte("line1_A\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtB, "shared.txt"),
		[]byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8_B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate conflicting binary __pycache__ artifacts produced inside each worktree.
	for _, pair := range []struct {
		dir  string
		data []byte
	}{
		{wtA, []byte("pyc-A")},
		{wtB, []byte("pyc-B")},
	} {
		cacheDir := filepath.Join(pair.dir, "app", "__pycache__")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "mod.cpython-312.pyc"), pair.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	target := filepath.Join(tmp, "merged")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "shared.txt"), []byte(baseShared), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(filepath.Join(tmp, ".soma"))
	rep, err := mgr.MergeBranchWorkspaces(ctx, repo, []string{wtA, wtB}, target)
	if err != nil {
		t.Fatalf("MergeBranchWorkspaces failed on real git worktrees: %v (conflicts=%v)", err, rep.Conflicts)
	}
	if len(rep.Conflicts) != 0 || len(rep.ThreeWayMerged) != 1 || rep.ThreeWayMerged[0] != "shared.txt" {
		t.Fatalf("unexpected merge report: %+v", rep)
	}
	merged, err := os.ReadFile(filepath.Join(target, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), "line1_A") || !strings.Contains(string(merged), "line8_B") {
		t.Fatalf("expected 3-way merged contents in target/shared.txt, got:\n%s", string(merged))
	}
	if _, err := os.Stat(filepath.Join(target, "app", "__pycache__", "mod.cpython-312.pyc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected __pycache__ artifacts not to be merged into target, got err=%v", err)
	}
}
