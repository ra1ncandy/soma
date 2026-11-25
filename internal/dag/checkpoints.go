package dag

import (
	"context"
	"errors"
	"fmt"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// Per-step workspace checkpoints. Unlike opt-in shadow-git snapshots, they are always on,
// content-addressed (unchanged files cost nothing), stored with the run, and tied to the DAG:
// forking or rewinding at any step restores exactly the files the branch had after that step.

// checkpointStart records the workspace a branch starts from (step "" = before any turn).
func (e *Executor) checkpointStart(ctx context.Context, run store.Run, branch store.Branch) {
	if e.Store == nil || branch.WorkspaceDir == "" {
		return
	}
	if _, err := e.Store.LatestCheckpoint(ctx, branch.ID); err == nil {
		return
	}
	e.checkpoint(ctx, run.ID, branch, "", nil)
}

// checkpointAfterTurn records the workspace after a turn that may have changed files.
func (e *Executor) checkpointAfterTurn(ctx context.Context, runID string, branch store.Branch, stepID string, calls []ToolCallSpec) {
	changed := false
	for _, c := range calls {
		cls := c.EffectClass
		if cls == "" {
			cls = inferToolEffectClass(c.Name)
		}
		if cls != taint.EffectReadOnly && cls != taint.EffectExternalRead {
			changed = true
			break
		}
	}
	if !changed {
		return
	}
	var prev workspace.CheckpointManifest
	if last, err := e.Store.LatestCheckpoint(ctx, branch.ID); err == nil {
		prev, _ = workspace.LoadCheckpoint(e.Store.Blobs(), last.ManifestHash)
	}
	e.checkpoint(ctx, runID, branch, stepID, prev)
}

func (e *Executor) checkpoint(ctx context.Context, runID string, branch store.Branch, stepID string, prev workspace.CheckpointManifest) {
	h, m, err := workspace.SnapshotWorkspace(e.Store.Blobs(), branch.WorkspaceDir, prev)
	if err != nil {
		return // a failed checkpoint must never fail the run
	}
	_ = e.Store.AddCheckpoint(context.WithoutCancel(ctx), store.WorkspaceCheckpoint{
		RunID: runID, BranchID: branch.ID, StepID: stepID, ManifestHash: h, Files: len(m),
	})
}

// checkpointAtStep finds the workspace state right after stepID: the newest checkpoint on the
// step's ancestry at or before it, else the start checkpoint of the branch the ancestry begins on.
func (e *Executor) checkpointAtStep(ctx context.Context, stepID string) (store.WorkspaceCheckpoint, error) {
	ancestry, err := e.Store.GetAncestry(ctx, stepID)
	if err != nil {
		return store.WorkspaceCheckpoint{}, err
	}
	for i := len(ancestry) - 1; i >= 0; i-- {
		if c, err := e.Store.GetCheckpoint(ctx, ancestry[i].BranchID, ancestry[i].ID); err == nil {
			return c, nil
		}
	}
	for _, s := range ancestry {
		if c, err := e.Store.GetCheckpoint(ctx, s.BranchID, ""); err == nil {
			return c, nil
		}
	}
	return store.WorkspaceCheckpoint{}, store.ErrNotFound
}

// RestoreStepFiles makes branchID's workspace match the files as they were after stepID.
func (e *Executor) RestoreStepFiles(ctx context.Context, branchID, stepID string) (workspace.RestoreReport, error) {
	br, err := e.Store.GetBranch(ctx, branchID)
	if err != nil {
		return workspace.RestoreReport{}, err
	}
	if br.WorkspaceDir == "" {
		return workspace.RestoreReport{}, errors.New("branch has no workspace")
	}
	cp, err := e.checkpointAtStep(ctx, stepID)
	if err != nil {
		return workspace.RestoreReport{}, fmt.Errorf("no workspace checkpoint at or before step %s: %w", stepID, err)
	}
	rep, err := workspace.RestoreWorkspace(e.Store.Blobs(), br.WorkspaceDir, cp.ManifestHash)
	if err != nil {
		return rep, err
	}
	_ = e.Store.AddCheckpoint(ctx, store.WorkspaceCheckpoint{RunID: br.RunID, BranchID: br.ID, StepID: stepID, ManifestHash: cp.ManifestHash, Files: cp.Files})
	return rep, nil
}
