package workspace

import (
	"context"
	"errors"
	"os"
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
