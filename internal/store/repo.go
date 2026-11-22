package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound indicates a requested record does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrNoCapacity indicates an account has reached its max_concurrent lease limit, budget, or cooldown.
	ErrNoCapacity = errors.New("store: account unavailable for lease")
	// ErrLeaseBusy indicates a remote resource lease is held by another active branch.
	ErrLeaseBusy = errors.New("store: remote resource lease held by another branch")
)

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

// --- Runs ---

// CreateRun inserts a new Run row.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	now := time.Now().Unix()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = r.CreatedAt
	}
	if r.WorkspaceKind == "" {
		r.WorkspaceKind = "generic"
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO runs (id, title, workspace_path, workspace_kind, goal_spec, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Title, r.WorkspacePath, r.WorkspaceKind, r.GoalSpec, r.Status, r.CreatedAt, r.UpdatedAt,
		)
		return err
	})
}

// GetRun fetches a Run by ID.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, title, workspace_path, workspace_kind, goal_spec, status, created_at, updated_at
		FROM runs WHERE id = ?`, id)
	var r Run
	if err := row.Scan(&r.ID, &r.Title, &r.WorkspacePath, &r.WorkspaceKind, &r.GoalSpec, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	return r, nil
}

// ListRuns returns runs ordered newest-first.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, title, workspace_path, workspace_kind, goal_spec, status, created_at, updated_at
		FROM runs ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Title, &r.WorkspacePath, &r.WorkspaceKind, &r.GoalSpec, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRunStatus updates a run's status and updated_at timestamp.
func (s *Store) UpdateRunStatus(ctx context.Context, id, status string) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE runs SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- Branches ---

// CreateBranch inserts a new Branch row.
func (s *Store) CreateBranch(ctx context.Context, b Branch) error {
	now := time.Now().Unix()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	if b.UpdatedAt == 0 {
		b.UpdatedAt = b.CreatedAt
	}
	if b.Epoch == 0 {
		b.Epoch = 1
	}
	if b.Status == "" {
		b.Status = "queued"
	}
	if b.WorkspaceStrategy == "" {
		b.WorkspaceStrategy = "copy_snapshot"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO branches (
				id, run_id, parent_branch_id, forked_from_step_id, label, head_step_id,
				epoch, status, workspace_dir, workspace_strategy, active_account_id, stall_count,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.ID, b.RunID, nullStr(b.ParentBranchID), nullStr(b.ForkedFromStepID),
			b.Label, nullStr(b.HeadStepID), b.Epoch, b.Status, b.WorkspaceDir, b.WorkspaceStrategy,
			nullStr(b.ActiveAccountID), b.StallCount, b.CreatedAt, b.UpdatedAt,
		)
		return err
	})
}

func scanBranch(sc rowScanner) (Branch, error) {
	var b Branch
	var parentBranch, forkedFrom, headStep, activeAcc sql.NullString
	err := sc.Scan(
		&b.ID, &b.RunID, &parentBranch, &forkedFrom, &b.Label, &headStep,
		&b.Epoch, &b.Status, &b.WorkspaceDir, &b.WorkspaceStrategy, &activeAcc,
		&b.StallCount, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return Branch{}, err
	}
	b.ParentBranchID = parentBranch.String
	b.ForkedFromStepID = forkedFrom.String
	b.HeadStepID = headStep.String
	b.ActiveAccountID = activeAcc.String
	return b, nil
}

// GetBranch fetches a Branch by ID.
func (s *Store) GetBranch(ctx context.Context, id string) (Branch, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, parent_branch_id, forked_from_step_id, label, head_step_id,
		       epoch, status, workspace_dir, workspace_strategy, active_account_id, stall_count,
		       created_at, updated_at
		FROM branches WHERE id = ?`, id)
	b, err := scanBranch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return b, err
}

// ListBranches returns branches belonging to runID (or all branches if runID is empty).
func (s *Store) ListBranches(ctx context.Context, runID string) ([]Branch, error) {
	query := `
		SELECT id, run_id, parent_branch_id, forked_from_step_id, label, head_step_id,
		       epoch, status, workspace_dir, workspace_strategy, active_account_id, stall_count,
		       created_at, updated_at
		FROM branches`
	var args []any
	if runID != "" {
		query += ` WHERE run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpdateBranchStatus updates a branch's status, active_account_id, and stall_count.
func (s *Store) UpdateBranchStatus(ctx context.Context, id, status string, activeAccountID string, stallCount int) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE branches
			SET status = ?, active_account_id = ?, stall_count = ?, updated_at = ?
			WHERE id = ?`,
			status, nullStr(activeAccountID), stallCount, now, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateBranchWorkspace updates a branch's workspace directory and strategy.
func (s *Store) UpdateBranchWorkspace(ctx context.Context, id, workspaceDir, strategy string) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE branches SET workspace_dir = ?, workspace_strategy = ?, updated_at = ? WHERE id = ?`,
			workspaceDir, strategy, now, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- Epoch Snapshots ---

// PutEpochSnapshot stores a frozen Layer 0/1 snapshot once per epoch.
func (s *Store) PutEpochSnapshot(ctx context.Context, snap EpochSnapshot) error {
	now := time.Now().Unix()
	if snap.CreatedAt == 0 {
		snap.CreatedAt = now
	}
	if snap.MetadataJSON == "" {
		snap.MetadataJSON = "{}"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO epoch_snapshots (
				id, run_id, branch_id, epoch, layer0_blob_hash, layer1_blob_hash,
				prefix_hash_l0_l1, metadata_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snap.ID, snap.RunID, snap.BranchID, snap.Epoch,
			snap.Layer0BlobHash, snap.Layer1BlobHash, snap.PrefixHashL0L1,
			snap.MetadataJSON, snap.CreatedAt,
		)
		return err
	})
}

func scanEpochSnapshot(sc rowScanner) (EpochSnapshot, error) {
	var es EpochSnapshot
	err := sc.Scan(
		&es.ID, &es.RunID, &es.BranchID, &es.Epoch,
		&es.Layer0BlobHash, &es.Layer1BlobHash, &es.PrefixHashL0L1,
		&es.MetadataJSON, &es.CreatedAt,
	)
	return es, err
}

// GetEpochSnapshot retrieves an EpochSnapshot by ID.
func (s *Store) GetEpochSnapshot(ctx context.Context, id string) (EpochSnapshot, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, branch_id, epoch, layer0_blob_hash, layer1_blob_hash,
		       prefix_hash_l0_l1, metadata_json, created_at
		FROM epoch_snapshots WHERE id = ?`, id)
	es, err := scanEpochSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EpochSnapshot{}, ErrNotFound
	}
	return es, err
}

// LatestEpochSnapshot returns the most recent EpochSnapshot for a branch and epoch.
func (s *Store) LatestEpochSnapshot(ctx context.Context, branchID string, epoch int) (EpochSnapshot, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, branch_id, epoch, layer0_blob_hash, layer1_blob_hash,
		       prefix_hash_l0_l1, metadata_json, created_at
		FROM epoch_snapshots
		WHERE branch_id = ? AND epoch = ?
		ORDER BY created_at DESC LIMIT 1`, branchID, epoch)
	es, err := scanEpochSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EpochSnapshot{}, ErrNotFound
	}
	return es, err
}

// --- Pages ---

// PutPage inserts a new Page index entry.
func (s *Store) PutPage(ctx context.Context, p Page) error {
	now := time.Now().Unix()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	if p.LastAccessedAt == 0 {
		p.LastAccessedAt = p.CreatedAt
	}
	if p.TrustTier == "" {
		p.TrustTier = "trusted_repo"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO pages (
				id, run_id, content_hash, blob_path, content_type, trust_tier,
				declassified, declassified_by, byte_size, line_count, preview_text,
				last_accessed_at, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.RunID, p.ContentHash, p.BlobPath, p.ContentType, p.TrustTier,
			boolToInt(p.Declassified), nullStr(p.DeclassifiedBy),
			p.ByteSize, p.LineCount, p.PreviewText, p.LastAccessedAt, p.CreatedAt,
		)
		return err
	})
}

// GetPage fetches a Page by ID.
func (s *Store) GetPage(ctx context.Context, id string) (Page, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, content_hash, blob_path, content_type, trust_tier,
		       declassified, declassified_by, byte_size, line_count, preview_text,
		       last_accessed_at, created_at
		FROM pages WHERE id = ?`, id)
	var p Page
	var declass int
	var declassBy sql.NullString
	if err := row.Scan(
		&p.ID, &p.RunID, &p.ContentHash, &p.BlobPath, &p.ContentType, &p.TrustTier,
		&declass, &declassBy, &p.ByteSize, &p.LineCount, &p.PreviewText,
		&p.LastAccessedAt, &p.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Page{}, ErrNotFound
		}
		return Page{}, err
	}
	p.Declassified = declass != 0
	p.DeclassifiedBy = declassBy.String
	return p, nil
}

// TouchPage updates last_accessed_at for a Page.
func (s *Store) TouchPage(ctx context.Context, id string) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE pages SET last_accessed_at = ? WHERE id = ?`, now, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- Steps & Action Journal ---

// BranchUpdate holds branch fields atomically updated when appending a Step.
type BranchUpdate struct {
	Epoch           int
	Status          string
	ActiveAccountID string
	StallCount      int
}

func appendStepTx(ctx context.Context, conn *sql.Conn, st Step, upd BranchUpdate) error {
	now := time.Now().Unix()
	if st.CreatedAt == 0 {
		st.CreatedAt = now
	}
	if st.InputProvenanceJSON == "" {
		st.InputProvenanceJSON = "[]"
	}
	if upd.Epoch == 0 {
		upd.Epoch = st.Epoch
	}
	if upd.Status == "" {
		upd.Status = "running"
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO steps (
			id, run_id, branch_id, parent_step_id, epoch, turn_index, account_id, model_id,
			state_hash, epoch_snapshot_id, frame_delta_blob_hash, compiled_frame_hash,
			emitted_blob_hash, observation_page_id, input_provenance_json,
			input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
			cost_usd, latency_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.ID, st.RunID, st.BranchID, nullStr(st.ParentStepID), st.Epoch, st.TurnIndex,
		st.AccountID, st.ModelID, st.StateHash, st.EpochSnapshotID, st.FrameDeltaBlobHash,
		st.CompiledFrameHash, st.EmittedBlobHash, nullStr(st.ObservationPageID), st.InputProvenanceJSON,
		st.InputTokens, st.CacheReadTokens, st.CacheWriteTokens, st.OutputTokens,
		st.CostUSD, st.LatencyMS, st.CreatedAt,
	); err != nil {
		return err
	}

	res, err := conn.ExecContext(ctx, `
		UPDATE branches
		SET head_step_id = ?, epoch = ?, status = ?, active_account_id = ?,
		    stall_count = ?, updated_at = ?
		WHERE id = ?`,
		st.ID, upd.Epoch, upd.Status, nullStr(upd.ActiveAccountID), upd.StallCount, now, st.BranchID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: branch %s", ErrNotFound, st.BranchID)
	}
	return nil
}

// AppendStep atomically inserts a Step and advances the owning branch's head pointer and state.
func (s *Store) AppendStep(ctx context.Context, st Step, upd BranchUpdate) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		return appendStepTx(ctx, conn, st, upd)
	})
}

// PrepareAction records an action in 'prepared' state before external dispatch.
func (s *Store) PrepareAction(ctx context.Context, entry ActionJournalEntry) error {
	now := time.Now().Unix()
	if entry.CreatedAt == 0 {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = entry.CreatedAt
	if entry.Status == "" {
		entry.Status = "prepared"
	}
	if entry.InputProvenanceJSON == "" {
		entry.InputProvenanceJSON = "[]"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO action_journal (
				action_id, run_id, branch_id, parent_step_id, epoch, turn_index,
				tool_name, args_json, args_hash, effect_class, idempotency_key,
				input_provenance_json, status, result_page_id, result_summary, error_text,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.ActionID, entry.RunID, entry.BranchID, nullStr(entry.ParentStepID),
			entry.Epoch, entry.TurnIndex, entry.ToolName, entry.ArgsJSON, entry.ArgsHash,
			entry.EffectClass, entry.IdempotencyKey, entry.InputProvenanceJSON, entry.Status,
			nullStr(entry.ResultPageID), nullStr(entry.ResultSummary), nullStr(entry.ErrorText),
			entry.CreatedAt, entry.UpdatedAt,
		)
		return err
	})
}

// MarkActionDispatched transitions a prepared action to 'dispatched' immediately prior to calling ToolRunner.
func (s *Store) MarkActionDispatched(ctx context.Context, actionID string) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE action_journal SET status = 'dispatched', updated_at = ?
			WHERE action_id = ?`, now, actionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// MarkActionStatus updates status, summary, and error fields on an ActionJournalEntry.
func (s *Store) MarkActionStatus(ctx context.Context, actionID, status, resultPageID, summary, errText string) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE action_journal
			SET status = ?, result_page_id = COALESCE(?, result_page_id),
			    result_summary = ?, error_text = ?, updated_at = ?
			WHERE action_id = ?`,
			status, nullStr(resultPageID), nullStr(summary), nullStr(errText), now, actionID,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CommitActionAndAppendStep atomically marks actionID recorded, inserts st, and updates branch head.
func (s *Store) CommitActionAndAppendStep(ctx context.Context, actionID, resultPageID, summary string, st Step, upd BranchUpdate) error {
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		if err := appendStepTx(ctx, conn, st, upd); err != nil {
			return err
		}
		if actionID != "" {
			res, err := conn.ExecContext(ctx, `
				UPDATE action_journal
				SET status = 'recorded', result_page_id = ?, result_summary = ?, updated_at = ?
				WHERE action_id = ?`,
				nullStr(resultPageID), nullStr(summary), now, actionID,
			)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: action %s", ErrNotFound, actionID)
			}
		}
		return nil
	})
}

func scanAction(sc rowScanner) (ActionJournalEntry, error) {
	var e ActionJournalEntry
	var parentStep, resultPage, summary, errText sql.NullString
	err := sc.Scan(
		&e.ActionID, &e.RunID, &e.BranchID, &parentStep, &e.Epoch, &e.TurnIndex,
		&e.ToolName, &e.ArgsJSON, &e.ArgsHash, &e.EffectClass, &e.IdempotencyKey,
		&e.InputProvenanceJSON, &e.Status, &resultPage, &summary, &errText,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return ActionJournalEntry{}, err
	}
	e.ParentStepID = parentStep.String
	e.ResultPageID = resultPage.String
	e.ResultSummary = summary.String
	e.ErrorText = errText.String
	return e, nil
}

const actionSelectCols = `
	action_id, run_id, branch_id, parent_step_id, epoch, turn_index,
	tool_name, args_json, args_hash, effect_class, idempotency_key,
	input_provenance_json, status, result_page_id, result_summary, error_text,
	created_at, updated_at`

// GetAction fetches an ActionJournalEntry by ID.
func (s *Store) GetAction(ctx context.Context, actionID string) (ActionJournalEntry, error) {
	row := s.reader.QueryRowContext(ctx, `SELECT `+actionSelectCols+` FROM action_journal WHERE action_id = ?`, actionID)
	e, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ActionJournalEntry{}, ErrNotFound
	}
	return e, err
}

// ListPendingActions returns unfinished or ambiguous actions for a branch (or all if branchID is empty).
func (s *Store) ListPendingActions(ctx context.Context, branchID string) ([]ActionJournalEntry, error) {
	query := `SELECT ` + actionSelectCols + ` FROM action_journal WHERE status IN ('prepared', 'dispatched', 'ambiguous')`
	var args []any
	if branchID != "" {
		query += ` AND branch_id = ?`
		args = append(args, branchID)
	}
	query += ` ORDER BY created_at ASC, turn_index ASC`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ActionJournalEntry
	for rows.Next() {
		e, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanStep(sc rowScanner) (Step, error) {
	var st Step
	var parentID, obsPageID sql.NullString
	err := sc.Scan(
		&st.ID, &st.RunID, &st.BranchID, &parentID, &st.Epoch, &st.TurnIndex,
		&st.AccountID, &st.ModelID, &st.StateHash, &st.EpochSnapshotID,
		&st.FrameDeltaBlobHash, &st.CompiledFrameHash, &st.EmittedBlobHash,
		&obsPageID, &st.InputProvenanceJSON, &st.InputTokens, &st.CacheReadTokens,
		&st.CacheWriteTokens, &st.OutputTokens, &st.CostUSD, &st.LatencyMS, &st.CreatedAt,
	)
	if err != nil {
		return Step{}, err
	}
	st.ParentStepID = parentID.String
	st.ObservationPageID = obsPageID.String
	return st, nil
}

const stepSelectCols = `
	id, run_id, branch_id, parent_step_id, epoch, turn_index, account_id, model_id,
	state_hash, epoch_snapshot_id, frame_delta_blob_hash, compiled_frame_hash,
	emitted_blob_hash, observation_page_id, input_provenance_json,
	input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
	cost_usd, latency_ms, created_at`

// GetStep fetches a single Step by ID.
func (s *Store) GetStep(ctx context.Context, id string) (Step, error) {
	row := s.reader.QueryRowContext(ctx, `SELECT `+stepSelectCols+` FROM steps WHERE id = ?`, id)
	st, err := scanStep(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, ErrNotFound
	}
	return st, err
}

// ListBranchSteps returns all steps directly created on branchID in execution order.
func (s *Store) ListBranchSteps(ctx context.Context, branchID string) ([]Step, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+stepSelectCols+` FROM steps WHERE branch_id = ? ORDER BY turn_index ASC, created_at ASC`,
		branchID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetAncestry walks parent_step_id links from headStepID back to the root step, returning chronological order.
func (s *Store) GetAncestry(ctx context.Context, headStepID string) ([]Step, error) {
	if headStepID == "" {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx, `
		WITH RECURSIVE chain(step_id, depth) AS (
			SELECT id, 0 FROM steps WHERE id = ?
			UNION ALL
			SELECT s.parent_step_id, c.depth + 1
			FROM steps s
			JOIN chain c ON s.id = c.step_id
			WHERE s.parent_step_id IS NOT NULL
		)
		SELECT `+prefixedStepCols("s")+`
		FROM chain c
		JOIN steps s ON s.id = c.step_id
		ORDER BY c.depth DESC`, headStepID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func prefixedStepCols(alias string) string {
	cols := []string{
		"id", "run_id", "branch_id", "parent_step_id", "epoch", "turn_index",
		"account_id", "model_id", "state_hash", "epoch_snapshot_id",
		"frame_delta_blob_hash", "compiled_frame_hash", "emitted_blob_hash",
		"observation_page_id", "input_provenance_json", "input_tokens",
		"cache_read_tokens", "cache_write_tokens", "output_tokens",
		"cost_usd", "latency_ms", "created_at",
	}
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ", ")
}

// --- Declassifications & Remote Leases ---

// AddDeclassification records a scoped declassification for a page and capability_scope.
func (s *Store) AddDeclassification(ctx context.Context, d Declassification) error {
	now := time.Now().Unix()
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO declassifications (id, page_id, capability_scope, method, approved_by, schema_name, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.PageID, d.CapabilityScope, d.Method, d.ApprovedBy, nullStr(d.SchemaName), d.CreatedAt,
		)
		return err
	})
}

// HasDeclassification returns true if pageID is globally declassified or explicitly authorized for capabilityScope.
func (s *Store) HasDeclassification(ctx context.Context, pageID, capabilityScope string) (bool, error) {
	var ok int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM pages WHERE id = ? AND declassified = 1
		UNION ALL
		SELECT 1 FROM declassifications WHERE page_id = ? AND (capability_scope = ? OR capability_scope = '*')
		LIMIT 1`, pageID, pageID, capabilityScope,
	).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AcquireRemoteLease acquires or extends a branch-exclusive remote lease with TTL seconds.
func (s *Store) AcquireRemoteLease(ctx context.Context, resourceKey, branchID, namespace string, ttl time.Duration) (RemoteLease, error) {
	now := time.Now().Unix()
	exp := now + int64(ttl.Seconds())
	if exp <= now {
		exp = now + 1
	}
	var out RemoteLease
	err := s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		var existingBranch, existingNS string
		var existingExp int64
		err := conn.QueryRowContext(ctx, `
			SELECT branch_id, remote_namespace, expires_at
			FROM remote_leases WHERE resource_key = ?`, resourceKey,
		).Scan(&existingBranch, &existingNS, &existingExp)

		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && existingExp > now && existingBranch != branchID {
			return fmt.Errorf("%w: %s held by %s until %d", ErrLeaseBusy, resourceKey, existingBranch, existingExp)
		}
		_, err = conn.ExecContext(ctx, `
			INSERT INTO remote_leases (resource_key, branch_id, remote_namespace, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(resource_key) DO UPDATE SET
				branch_id = excluded.branch_id,
				remote_namespace = excluded.remote_namespace,
				expires_at = excluded.expires_at,
				created_at = excluded.created_at`,
			resourceKey, branchID, namespace, exp, now,
		)
		if err != nil {
			return err
		}
		out = RemoteLease{
			ResourceKey:     resourceKey,
			BranchID:        branchID,
			RemoteNamespace: namespace,
			ExpiresAt:       exp,
			CreatedAt:       now,
		}
		return nil
	})
	return out, err
}

// ReleaseRemoteLease releases a remote lease held by branchID.
func (s *Store) ReleaseRemoteLease(ctx context.Context, resourceKey, branchID string) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			DELETE FROM remote_leases WHERE resource_key = ? AND branch_id = ?`,
			resourceKey, branchID,
		)
		return err
	})
}

// --- Memory Graph ---

func scanMemoryNode(sc rowScanner) (MemoryNode, error) {
	var m MemoryNode
	var sig, provStep, evPage sql.NullString
	var validTo sql.NullInt64
	var enabled int
	if err := sc.Scan(
		&m.ID, &m.Scope, &m.Kind, &m.Content, &sig, &m.Status, &m.TrustTier,
		&provStep, &evPage, &m.ValidFrom, &validTo, &m.IngestedAt, &enabled,
	); err != nil {
		return MemoryNode{}, err
	}
	m.SignatureHash = sig.String
	m.ProvenanceStepID = provStep.String
	m.EvidencePageID = evPage.String
	if validTo.Valid {
		v := validTo.Int64
		m.ValidTo = &v
	}
	m.Enabled = enabled != 0
	return m, nil
}

func insertMemoryNodeTx(ctx context.Context, conn *sql.Conn, m MemoryNode) error {
	now := time.Now().Unix()
	if m.ValidFrom == 0 {
		m.ValidFrom = now
	}
	if m.IngestedAt == 0 {
		m.IngestedAt = now
	}
	if m.Status == "" {
		m.Status = "active"
	}
	if m.TrustTier == "" {
		m.TrustTier = "trusted_user"
	}
	_, err := conn.ExecContext(ctx, `
		INSERT INTO memory_nodes (
			id, scope, kind, content, signature_hash, status, trust_tier,
			provenance_step_id, evidence_page_id, valid_from, valid_to, ingested_at, enabled
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Scope, m.Kind, m.Content, nullStr(m.SignatureHash), m.Status, m.TrustTier,
		nullStr(m.ProvenanceStepID), nullStr(m.EvidencePageID),
		m.ValidFrom, m.ValidTo, m.IngestedAt, boolToInt(m.Enabled),
	)
	return err
}

func queryMemoryRows(ctx context.Context, db *sql.DB, baseWhere string, scopes, kinds []string, baseArgs ...any) ([]MemoryNode, error) {
	query := `
		SELECT id, scope, kind, content, signature_hash, status, trust_tier,
		       provenance_step_id, evidence_page_id, valid_from, valid_to, ingested_at, enabled
		FROM memory_nodes WHERE ` + baseWhere
	args := append([]any(nil), baseArgs...)

	if len(scopes) > 0 {
		placeholders := strings.Repeat("?,", len(scopes))
		query += ` AND scope IN (` + placeholders[:len(placeholders)-1] + `)`
		for _, sc := range scopes {
			args = append(args, sc)
		}
	}
	if len(kinds) > 0 {
		placeholders := strings.Repeat("?,", len(kinds))
		query += ` AND kind IN (` + placeholders[:len(placeholders)-1] + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	query += ` ORDER BY id ASC`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MemoryNode
	for rows.Next() {
		m, err := scanMemoryNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InsertMemoryNode inserts a bi-temporal rule, fact, preference, or negative_path node.
func (s *Store) InsertMemoryNode(ctx context.Context, m MemoryNode) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		return insertMemoryNodeTx(ctx, conn, m)
	})
}

// GetMemoryNode fetches a MemoryNode by ID.
func (s *Store) GetMemoryNode(ctx context.Context, id string) (MemoryNode, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, scope, kind, content, signature_hash, status, trust_tier,
		       provenance_step_id, evidence_page_id, valid_from, valid_to, ingested_at, enabled
		FROM memory_nodes WHERE id = ?`, id)
	m, err := scanMemoryNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryNode{}, ErrNotFound
	}
	return m, err
}

// SupersedeMemoryNode atomically invalidates oldID, inserts newNode, and records a SUPERSEDES edge.
func (s *Store) SupersedeMemoryNode(ctx context.Context, oldID string, newNode MemoryNode, provenanceStepID string) error {
	return s.MergeMemoryNodes(ctx, []string{oldID}, newNode, provenanceStepID)
}

// MergeMemoryNodes atomically supersedes oldIDs into mergedNode with SUPERSEDES (1-to-1) or MERGES (N-to-1) edges.
func (s *Store) MergeMemoryNodes(ctx context.Context, oldIDs []string, mergedNode MemoryNode, provenanceStepID string) error {
	if len(oldIDs) == 0 {
		return errors.New("MergeMemoryNodes requires at least one oldID")
	}
	now := time.Now().Unix()
	if mergedNode.ValidFrom == 0 {
		mergedNode.ValidFrom = now
	}
	if mergedNode.IngestedAt == 0 {
		mergedNode.IngestedAt = now
	}
	if mergedNode.Status == "" {
		mergedNode.Status = "active"
	}
	rel := "MERGES"
	if len(oldIDs) == 1 {
		rel = "SUPERSEDES"
	}
	validTo := mergedNode.ValidFrom

	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		if err := insertMemoryNodeTx(ctx, conn, mergedNode); err != nil {
			return err
		}
		for _, oldID := range oldIDs {
			res, err := conn.ExecContext(ctx, `
				UPDATE memory_nodes SET status = 'superseded', valid_to = ? WHERE id = ?`,
				validTo, oldID,
			)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: old memory node %s", ErrNotFound, oldID)
			}
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO memory_edges (from_node_id, to_node_id, relation, provenance_step_id, created_at)
				VALUES (?, ?, ?, ?, ?)`,
				mergedNode.ID, oldID, rel, nullStr(provenanceStepID), mergedNode.IngestedAt,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// ActivateMemoryNode transitions a 'proposed' memory node to 'active'.
func (s *Store) ActivateMemoryNode(ctx context.Context, id string) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE memory_nodes SET status = 'active' WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ArchiveMemoryNode marks a memory node 'archived' at validTo.
func (s *Store) ArchiveMemoryNode(ctx context.Context, id string, validTo int64) error {
	if validTo == 0 {
		validTo = time.Now().Unix()
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE memory_nodes SET status = 'archived', valid_to = ? WHERE id = ?`, validTo, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListActiveMemory returns active, enabled, un-invalidated memory nodes ordered deterministically by ID.
func (s *Store) ListActiveMemory(ctx context.Context, scope string, kinds []string) ([]MemoryNode, error) {
	var scopes []string
	if scope != "" {
		scopes = []string{scope}
	}
	return s.ListActiveMemoryByScopes(ctx, scopes, kinds)
}

// ListActiveMemoryByScopes returns active memory nodes across multiple scopes in deterministic ID order.
func (s *Store) ListActiveMemoryByScopes(ctx context.Context, scopes []string, kinds []string) ([]MemoryNode, error) {
	return queryMemoryRows(ctx, s.reader, "status = 'active' AND valid_to IS NULL AND enabled = 1", scopes, kinds)
}

// QueryMemoryAsOf returns enabled non-proposed memory nodes valid at point-in-time asOfUnix.
func (s *Store) QueryMemoryAsOf(ctx context.Context, scopes []string, kinds []string, asOfUnix int64) ([]MemoryNode, error) {
	return queryMemoryRows(
		ctx,
		s.reader,
		"valid_from <= ? AND (valid_to IS NULL OR valid_to > ?) AND status != 'proposed' AND enabled = 1",
		scopes,
		kinds,
		asOfUnix,
		asOfUnix,
	)
}

// ListMemoryEdges returns all edges touching nodeID.
func (s *Store) ListMemoryEdges(ctx context.Context, nodeID string) ([]MemoryEdge, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, from_node_id, to_node_id, relation, provenance_step_id, created_at
		FROM memory_edges
		WHERE from_node_id = ? OR to_node_id = ?
		ORDER BY id ASC`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MemoryEdge
	for rows.Next() {
		var e MemoryEdge
		var prov sql.NullString
		if err := rows.Scan(&e.ID, &e.FromNodeID, &e.ToNodeID, &e.Relation, &prov, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.ProvenanceStepID = prov.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddMemoryEdge inserts a directed edge between two memory nodes.
func (s *Store) AddMemoryEdge(ctx context.Context, edge MemoryEdge) error {
	now := time.Now().Unix()
	if edge.CreatedAt == 0 {
		edge.CreatedAt = now
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO memory_edges (from_node_id, to_node_id, relation, provenance_step_id, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			edge.FromNodeID, edge.ToNodeID, edge.Relation, nullStr(edge.ProvenanceStepID), edge.CreatedAt,
		)
		return err
	})
}

// --- Accounts & Quota Leases ---

// UpsertAccount creates or updates an Account configuration record.
func (s *Store) UpsertAccount(ctx context.Context, a Account) error {
	now := time.Now().Unix()
	if a.UpdatedAt == 0 {
		a.UpdatedAt = now
	}
	if a.MaxConcurrent <= 0 {
		a.MaxConcurrent = 1
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO accounts (
				id, provider, model_id, billing_mode, max_concurrent, active_leases,
				cooldown_until, daily_budget_usd, daily_spent_usd, last_error, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				provider = excluded.provider,
				model_id = excluded.model_id,
				billing_mode = excluded.billing_mode,
				max_concurrent = excluded.max_concurrent,
				daily_budget_usd = excluded.daily_budget_usd,
				updated_at = excluded.updated_at`,
			a.ID, a.Provider, a.ModelID, a.BillingMode, a.MaxConcurrent, a.ActiveLeases,
			a.CooldownUntil, a.DailyBudgetUSD, a.DailySpentUSD, nullStr(a.LastError), a.UpdatedAt,
		)
		return err
	})
}

func scanAccount(sc rowScanner) (Account, error) {
	var a Account
	var lastErr sql.NullString
	err := sc.Scan(
		&a.ID, &a.Provider, &a.ModelID, &a.BillingMode, &a.MaxConcurrent, &a.ActiveLeases,
		&a.CooldownUntil, &a.DailyBudgetUSD, &a.DailySpentUSD, &lastErr, &a.UpdatedAt,
	)
	if err != nil {
		return Account{}, err
	}
	a.LastError = lastErr.String
	return a, nil
}

// GetAccount fetches an Account by ID.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, provider, model_id, billing_mode, max_concurrent, active_leases,
		       cooldown_until, daily_budget_usd, daily_spent_usd, last_error, updated_at
		FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// ListAccounts returns all configured accounts ordered by ID.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, provider, model_id, billing_mode, max_concurrent, active_leases,
		       cooldown_until, daily_budget_usd, daily_spent_usd, last_error, updated_at
		FROM accounts ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AcquireLease atomically increments active_leases if off cooldown (in Unix milliseconds) and below max_concurrent.
func (s *Store) AcquireLease(ctx context.Context, id string, nowUnixMs int64) error {
	nowSec := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		var exists int
		if err := conn.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		res, err := conn.ExecContext(ctx, `
			UPDATE accounts
			SET active_leases = active_leases + 1, updated_at = ?
			WHERE id = ?
			  AND active_leases < max_concurrent
			  AND cooldown_until <= ?
			  AND (billing_mode != 'metered' OR daily_budget_usd <= 0 OR daily_spent_usd < daily_budget_usd)`,
			nowSec, id, nowUnixMs,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNoCapacity
		}
		return nil
	})
}

// ReleaseLease decrements active_leases (clamped at 0) and records metered spend delta.
func (s *Store) ReleaseLease(ctx context.Context, id string, spentDeltaUSD float64) error {
	nowSec := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE accounts
			SET active_leases = CASE WHEN active_leases > 0 THEN active_leases - 1 ELSE 0 END,
			    daily_spent_usd = daily_spent_usd + ?,
			    updated_at = ?
			WHERE id = ?`,
			spentDeltaUSD, nowSec, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetCooldown marks an account as rate-limited until cooldownUntilUnixMs and records the error reason.
func (s *Store) SetCooldown(ctx context.Context, id string, cooldownUntilUnixMs int64, lastError string) error {
	nowSec := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE accounts
			SET cooldown_until = ?, last_error = ?, updated_at = ?
			WHERE id = ?`,
			cooldownUntilUnixMs, nullStr(lastError), nowSec, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- Inbox ---

// InsertInboxItem inserts a pending operator inbox item.
func (s *Store) InsertInboxItem(ctx context.Context, item InboxItem) error {
	now := time.Now().Unix()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	if item.Urgency == "" {
		item.Urgency = "normal"
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO inbox_items (
				id, run_id, branch_id, trigger_id, item_kind, title, summary,
				artifact_ref, urgency, status, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, nullStr(item.RunID), nullStr(item.BranchID), nullStr(item.TriggerID),
			item.ItemKind, item.Title, item.Summary, nullStr(item.ArtifactRef),
			item.Urgency, item.Status, item.CreatedAt,
		)
		return err
	})
}

// ListInboxItems returns inbox items filtered by status (or all if status is empty).
func (s *Store) ListInboxItems(ctx context.Context, status string) ([]InboxItem, error) {
	query := `
		SELECT id, run_id, branch_id, trigger_id, item_kind, title, summary,
		       artifact_ref, urgency, status, created_at
		FROM inbox_items`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC, id ASC`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		var runID, branchID, trigID, artRef sql.NullString
		if err := rows.Scan(
			&it.ID, &runID, &branchID, &trigID, &it.ItemKind, &it.Title,
			&it.Summary, &artRef, &it.Urgency, &it.Status, &it.CreatedAt,
		); err != nil {
			return nil, err
		}
		it.RunID = runID.String
		it.BranchID = branchID.String
		it.TriggerID = trigID.String
		it.ArtifactRef = artRef.String
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateInboxStatus updates an InboxItem status.
func (s *Store) UpdateInboxStatus(ctx context.Context, id, status string) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE inbox_items SET status = ? WHERE id = ?`, status, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- Verification Contracts ---

// CreateContract persists a verification contract attached to a Run.
func (s *Store) CreateContract(ctx context.Context, c Contract) error {
	now := time.Now().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO contracts (id, run_id, kind, spec_json, pinned_hash, required, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.RunID, c.Kind, c.SpecJSON, c.PinnedHash, boolToInt(c.Required), c.CreatedAt,
		)
		return err
	})
}

// ListRunContracts returns all contracts bound to runID ordered by creation time and ID.
func (s *Store) ListRunContracts(ctx context.Context, runID string) ([]Contract, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, run_id, kind, spec_json, pinned_hash, required, created_at
		FROM contracts WHERE run_id = ? ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Contract
	for rows.Next() {
		var c Contract
		var req int
		if err := rows.Scan(&c.ID, &c.RunID, &c.Kind, &c.SpecJSON, &c.PinnedHash, &req, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Required = req != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordContractRun stores an evaluation record for a contract run.
func (s *Store) RecordContractRun(ctx context.Context, cr ContractRun) error {
	now := time.Now().Unix()
	if cr.CreatedAt == 0 {
		cr.CreatedAt = now
	}
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO contract_runs (
				id, contract_id, branch_id, step_id, passed, output_page_id,
				artifact_sha256, counterexample_text, duration_ms, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cr.ID, cr.ContractID, cr.BranchID, cr.StepID, boolToInt(cr.Passed),
			nullStr(cr.OutputPageID), nullStr(cr.ArtifactSHA256),
			nullStr(cr.CounterexampleText), cr.DurationMS, cr.CreatedAt,
		)
		return err
	})
}

// ListBranchContractRuns returns contract evaluation history for branchID.
func (s *Store) ListBranchContractRuns(ctx context.Context, branchID string) ([]ContractRun, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, contract_id, branch_id, step_id, passed, output_page_id,
		       artifact_sha256, counterexample_text, duration_ms, created_at
		FROM contract_runs WHERE branch_id = ? ORDER BY created_at ASC, id ASC`, branchID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ContractRun
	for rows.Next() {
		var cr ContractRun
		var passed int
		var outPage, sha, ce sql.NullString
		if err := rows.Scan(
			&cr.ID, &cr.ContractID, &cr.BranchID, &cr.StepID, &passed,
			&outPage, &sha, &ce, &cr.DurationMS, &cr.CreatedAt,
		); err != nil {
			return nil, err
		}
		cr.Passed = passed != 0
		cr.OutputPageID = outPage.String
		cr.ArtifactSHA256 = sha.String
		cr.CounterexampleText = ce.String
		out = append(out, cr)
	}
	return out, rows.Err()
}

// --- Trigger Runs (Tier-0 Event Daemon) ---

func scanTriggerRun(sc rowScanner) (TriggerRun, error) {
	var tr TriggerRun
	var hash, brID sql.NullString
	if err := sc.Scan(
		&tr.IdempotencyKey, &tr.TriggerID, &hash, &brID,
		&tr.Status, &tr.CostUSD, &tr.CreatedAt, &tr.ExpiresAt,
	); err != nil {
		return TriggerRun{}, err
	}
	tr.LastSentinelHash = hash.String
	tr.BranchID = brID.String
	return tr, nil
}

// TryClaimTriggerRun atomically claims idempotency_key if unseen; returns (true, tr, nil) on claim,
// or (false, existing, nil) if already recorded.
func (s *Store) TryClaimTriggerRun(ctx context.Context, tr TriggerRun) (bool, TriggerRun, error) {
	now := time.Now().Unix()
	if tr.CreatedAt == 0 {
		tr.CreatedAt = now
	}
	if tr.ExpiresAt == 0 {
		tr.ExpiresAt = tr.CreatedAt + 7*24*3600
	}
	if tr.Status == "" {
		tr.Status = "running"
	}

	var claimed bool
	var out TriggerRun
	err := s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO trigger_runs (
				idempotency_key, trigger_id, last_sentinel_hash, branch_id,
				status, cost_usd, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			tr.IdempotencyKey, tr.TriggerID, nullStr(tr.LastSentinelHash), nullStr(tr.BranchID),
			tr.Status, tr.CostUSD, tr.CreatedAt, tr.ExpiresAt,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			claimed = true
			out = tr
			return nil
		}
		row := conn.QueryRowContext(ctx, `
			SELECT idempotency_key, trigger_id, last_sentinel_hash, branch_id,
			       status, cost_usd, created_at, expires_at
			FROM trigger_runs WHERE idempotency_key = ?`, tr.IdempotencyKey)
		existing, err := scanTriggerRun(row)
		if err != nil {
			return err
		}
		out = existing
		return nil
	})
	return claimed, out, err
}

// UpdateTriggerRun updates status, branch link, sentinel hash, and metered cost for a trigger_runs entry.
func (s *Store) UpdateTriggerRun(ctx context.Context, idempotencyKey, status, branchID, sentinelHash string, costUSD float64) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE trigger_runs
			SET status = ?,
			    branch_id = COALESCE(?, branch_id),
			    last_sentinel_hash = COALESCE(?, last_sentinel_hash),
			    cost_usd = ?
			WHERE idempotency_key = ?`,
			status, nullStr(branchID), nullStr(sentinelHash), costUSD, idempotencyKey,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetLatestTriggerHash returns the most recent non-empty sentinel output hash for triggerID.
func (s *Store) GetLatestTriggerHash(ctx context.Context, triggerID string) (string, error) {
	var h sql.NullString
	err := s.reader.QueryRowContext(ctx, `
		SELECT last_sentinel_hash
		FROM trigger_runs
		WHERE trigger_id = ? AND last_sentinel_hash IS NOT NULL AND last_sentinel_hash != ''
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1`, triggerID,
	).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h.String, nil
}

// ListTriggerRuns lists recent trigger_runs entries optionally filtered by triggerID.
func (s *Store) ListTriggerRuns(ctx context.Context, triggerID string, limit int) ([]TriggerRun, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT idempotency_key, trigger_id, last_sentinel_hash, branch_id,
		       status, cost_usd, created_at, expires_at
		FROM trigger_runs`
	var args []any
	if triggerID != "" {
		query += ` WHERE trigger_id = ?`
		args = append(args, triggerID)
	}
	query += ` ORDER BY created_at DESC, idempotency_key ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []TriggerRun
	for rows.Next() {
		tr, err := scanTriggerRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// PurgeExpiredTriggerRuns garbage-collects trigger_runs whose expires_at <= nowUnix.
func (s *Store) PurgeExpiredTriggerRuns(ctx context.Context, nowUnix int64) (int64, error) {
	var deleted int64
	err := s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `DELETE FROM trigger_runs WHERE expires_at <= ?`, nowUnix)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		deleted = n
		return nil
	})
	return deleted, err
}

// --- Memory, Account, Page & Inbox Helpers (v0.3) ---

// ListAllMemoryNodes returns memory nodes across all lifecycle statuses ordered by priority/recency.
func (s *Store) ListAllMemoryNodes(ctx context.Context, scope string) ([]MemoryNode, error) {
	query := `
		SELECT id, scope, kind, content, signature_hash, status, trust_tier,
		       provenance_step_id, evidence_page_id, valid_from, valid_to, ingested_at, enabled
		FROM memory_nodes`
	var args []any
	if scope != "" {
		query += ` WHERE scope = ?`
		args = append(args, scope)
	}
	query += `
		ORDER BY CASE status WHEN 'proposed' THEN 0 WHEN 'active' THEN 1 ELSE 2 END,
		         ingested_at DESC, id ASC`

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []MemoryNode
	for rows.Next() {
		m, err := scanMemoryNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemoryEnabled toggles whether a memory node is injected into new epochs.
func (s *Store) SetMemoryEnabled(ctx context.Context, id string, enabled bool) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE memory_nodes SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateMemoryContent updates the textual content of a memory node.
func (s *Store) UpdateMemoryContent(ctx context.Context, id, content string) error {
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE memory_nodes SET content = ? WHERE id = ?`, content, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ResetCooldown clears an account's rate-limit cooldown timer and error text.
func (s *Store) ResetCooldown(ctx context.Context, id string) error {
	nowSec := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			UPDATE accounts
			SET cooldown_until = 0, last_error = NULL, updated_at = ?
			WHERE id = ?`, nowSec, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// TotalDailyMeteredSpend returns the aggregate daily spend across metered accounts.
func (s *Store) TotalDailyMeteredSpend(ctx context.Context) (float64, error) {
	var total float64
	err := s.reader.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(daily_spent_usd), 0.0)
		FROM accounts WHERE billing_mode = 'metered'`,
	).Scan(&total)
	return total, err
}

// ListRunPages lists spilled @page entries for runID newest-first.
func (s *Store) ListRunPages(ctx context.Context, runID string, limit int) ([]Page, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT id, run_id, content_hash, blob_path, content_type, trust_tier,
		       declassified, declassified_by, byte_size, line_count, preview_text,
		       last_accessed_at, created_at
		FROM pages`
	var args []any
	if runID != "" {
		query += ` WHERE run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY created_at DESC, id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Page
	for rows.Next() {
		var p Page
		var declass int
		var declassBy sql.NullString
		if err := rows.Scan(
			&p.ID, &p.RunID, &p.ContentHash, &p.BlobPath, &p.ContentType, &p.TrustTier,
			&declass, &declassBy, &p.ByteSize, &p.LineCount, &p.PreviewText,
			&p.LastAccessedAt, &p.CreatedAt,
		); err != nil {
			return nil, err
		}
		p.Declassified = declass != 0
		p.DeclassifiedBy = declassBy.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetInboxItem retrieves a single InboxItem by ID.
func (s *Store) GetInboxItem(ctx context.Context, id string) (InboxItem, error) {
	var it InboxItem
	var runID, branchID, trigID, artRef sql.NullString
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, run_id, branch_id, trigger_id, item_kind, title, summary,
		       artifact_ref, urgency, status, created_at
		FROM inbox_items WHERE id = ?`, id,
	).Scan(
		&it.ID, &runID, &branchID, &trigID, &it.ItemKind, &it.Title,
		&it.Summary, &artRef, &it.Urgency, &it.Status, &it.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxItem{}, ErrNotFound
	}
	if err != nil {
		return InboxItem{}, err
	}
	it.RunID = runID.String
	it.BranchID = branchID.String
	it.TriggerID = trigID.String
	it.ArtifactRef = artRef.String
	return it, nil
}

// CountPendingInbox returns the number of inbox items currently in 'pending' status.
func (s *Store) CountPendingInbox(ctx context.Context) (int, error) {
	var count int
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE status = 'pending'`).Scan(&count)
	return count, err
}
