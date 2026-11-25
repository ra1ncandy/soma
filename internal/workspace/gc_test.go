package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func TestGCWorkspacesKeepsUnappliedWorkAndRemovesTheRest(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	mustWrite(t, filepath.Join(project, "a.txt"), "base\n")
	somaDir := filepath.Join(project, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := NewManager(somaDir)

	run := store.Run{ID: "run1", Title: "t", WorkspacePath: project, GoalSpec: "g"}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	mk := func(id, status string) string {
		br := store.Branch{ID: id, RunID: run.ID, Label: id, Status: status, WorkspaceStrategy: "copy_snapshot", CreatedAt: 1, UpdatedAt: 1}
		if err := st.CreateBranch(ctx, br); err != nil {
			t.Fatal(err)
		}
		dir, _, err := m.PrepareBranchWorkspace(ctx, st, run, br)
		if err != nil {
			t.Fatal(err)
		}
		// PrepareBranchWorkspace bumps updated_at; age the branch again.
		if err := st.UpdateBranchStatus(ctx, id, status, "", 0); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	clean := mk("br_clean", "completed")
	dirty := mk("br_dirty", "completed")
	live := mk("br_live", "running")
	mustWrite(t, filepath.Join(dirty, "a.txt"), "unapplied edit\n")
	orphan := filepath.Join(somaDir, "workspaces", "br_orphan")
	mustWrite(t, filepath.Join(orphan, "x.txt"), "x")

	// minAge of 0 treats everything as old enough.
	rep, err := m.GCWorkspaces(ctx, st, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exists(clean) {
		t.Errorf("finished branch with no pending changes should be collected (report %+v)", rep)
	}
	if exists(orphan) {
		t.Error("orphaned workspace should be collected")
	}
	if !exists(dirty) {
		t.Error("finished branch with unapplied changes must be kept")
	}
	if !exists(live) {
		t.Error("running branch workspace must be kept")
	}
	br, err := st.GetBranch(ctx, "br_clean")
	if err != nil || br.WorkspaceDir != "" {
		t.Errorf("collected branch should have workspace_dir cleared, got %q err=%v", br.WorkspaceDir, err)
	}
	if _, err := os.Stat(baseManifestPath(clean)); err == nil {
		t.Error("manifest of a collected workspace should be removed")
	}
}
