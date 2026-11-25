package workspace

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// GCReport summarises one workspace garbage-collection pass.
type GCReport struct {
	Removed    []string `json:"removed,omitempty"`
	FreedBytes int64    `json:"freed_bytes"`
	Kept       int      `json:"kept"`
}

// terminalBranchStatus reports statuses after which a branch no longer needs its files unless it
// still holds changes that were never applied.
func terminalBranchStatus(status string) bool {
	switch status {
	case "completed", "failed", "abandoned", "stopped":
		return true
	}
	return false
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// GCWorkspaces removes branch workspaces that can no longer matter:
//   - directories whose branch row is gone (deleted runs), and
//   - workspaces of finished branches, idle for at least minAge, that hold no unapplied changes.
//
// A finished branch that still has changes the operator has not applied is always kept. A
// collected branch has its workspace_dir cleared, so continuing it later provisions a fresh
// workspace from the project. isActive lets the caller protect branches that are executing.
func (m *Manager) GCWorkspaces(ctx context.Context, st *store.Store, minAge time.Duration, isActive func(branchID string) bool) (GCReport, error) {
	var rep GCReport
	root := filepath.Join(m.somaDir, "workspaces")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, err
	}
	branches, err := st.ListBranches(ctx, "")
	if err != nil {
		return rep, err
	}
	byID := make(map[string]store.Branch, len(branches))
	for _, b := range branches {
		byID[b.ID] = b
	}
	runCache := make(map[string]store.Run)
	cutoff := time.Now().Add(-minAge).Unix()
	prunedWorktrees := make(map[string]bool)

	for _, ent := range entries {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		if !ent.IsDir() {
			// Manifests of workspaces that no longer exist.
			name := ent.Name()
			if id, ok := strings.CutSuffix(name, ".base.json"); ok {
				if _, sErr := os.Stat(filepath.Join(root, id)); os.IsNotExist(sErr) {
					_ = os.Remove(filepath.Join(root, name))
				}
			} else if id, ok := strings.CutSuffix(name, ".fork.json"); ok {
				if _, sErr := os.Stat(filepath.Join(root, id)); os.IsNotExist(sErr) {
					_ = os.Remove(filepath.Join(root, name))
				}
			}
			continue
		}
		dir := filepath.Join(root, ent.Name())
		br, known := byID[ent.Name()]
		if !known {
			// Orphan: its run was deleted. Only collect once it is old enough that it cannot be a
			// workspace being created right now for a branch row we listed a moment too early.
			if info, iErr := ent.Info(); iErr == nil && info.ModTime().Unix() > cutoff {
				rep.Kept++
				continue
			}
			size := dirSize(dir)
			_ = os.Remove(baseManifestPath(dir))
			_ = os.Remove(forkManifestPath(dir))
			if os.RemoveAll(dir) == nil {
				rep.Removed = append(rep.Removed, ent.Name())
				rep.FreedBytes += size
			}
			continue
		}
		if !terminalBranchStatus(br.Status) || br.UpdatedAt > cutoff || br.WorkspaceStrategy == StrategyDirect ||
			!sameCleanedPath(br.WorkspaceDir, dir) || (isActive != nil && isActive(br.ID)) {
			rep.Kept++
			continue
		}
		run, ok := runCache[br.RunID]
		if !ok {
			run, err = st.GetRun(ctx, br.RunID)
			if err != nil {
				rep.Kept++
				continue
			}
			runCache[br.RunID] = run
		}
		if m.CountWorkspaceChanges(ctx, run.WorkspacePath, dir) > 0 {
			rep.Kept++ // unapplied work: the operator may still want to review or apply it
			continue
		}
		size := dirSize(dir)
		if err := m.CleanupBranchWorkspace(ctx, run, br); err != nil {
			rep.Kept++
			continue
		}
		_ = st.UpdateBranchWorkspace(ctx, br.ID, "", br.WorkspaceStrategy)
		rep.Removed = append(rep.Removed, ent.Name())
		rep.FreedBytes += size
		if br.WorkspaceStrategy == StrategyGitWorktree && run.WorkspacePath != "" {
			prunedWorktrees[run.WorkspacePath] = true
		}
	}
	for repo := range prunedWorktrees {
		_ = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune").Run()
	}
	return rep, nil
}
