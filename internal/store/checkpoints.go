package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// WorkspaceCheckpoint links a step (or, with an empty StepID, the start of a branch) to the
// content-addressed manifest of the branch workspace at that point.
type WorkspaceCheckpoint struct {
	RunID        string
	BranchID     string
	StepID       string
	ManifestHash string
	Files        int
	CreatedAt    int64
}

// AddCheckpoint records a workspace checkpoint.
func (s *Store) AddCheckpoint(ctx context.Context, c WorkspaceCheckpoint) error {
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().UnixNano()
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO workspace_checkpoints(run_id, branch_id, step_id, manifest_hash, files, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			c.RunID, c.BranchID, c.StepID, c.ManifestHash, c.Files, c.CreatedAt)
		return err
	})
}

// GetCheckpoint returns the latest checkpoint recorded for (branchID, stepID).
func (s *Store) GetCheckpoint(ctx context.Context, branchID, stepID string) (WorkspaceCheckpoint, error) {
	var c WorkspaceCheckpoint
	err := s.reader.QueryRowContext(ctx, `SELECT run_id, branch_id, step_id, manifest_hash, files, created_at
		FROM workspace_checkpoints WHERE branch_id = ? AND step_id = ? ORDER BY id DESC LIMIT 1`, branchID, stepID).
		Scan(&c.RunID, &c.BranchID, &c.StepID, &c.ManifestHash, &c.Files, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// LatestCheckpoint returns the most recent checkpoint of a branch.
func (s *Store) LatestCheckpoint(ctx context.Context, branchID string) (WorkspaceCheckpoint, error) {
	var c WorkspaceCheckpoint
	err := s.reader.QueryRowContext(ctx, `SELECT run_id, branch_id, step_id, manifest_hash, files, created_at
		FROM workspace_checkpoints WHERE branch_id = ? ORDER BY id DESC LIMIT 1`, branchID).
		Scan(&c.RunID, &c.BranchID, &c.StepID, &c.ManifestHash, &c.Files, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CheckpointManifestHashes lists every checkpoint manifest (for blob garbage collection).
func (s *Store) CheckpointManifestHashes(ctx context.Context) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT DISTINCT manifest_hash FROM workspace_checkpoints`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
