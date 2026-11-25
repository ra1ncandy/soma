package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// versionedMigrations run once each, in order, tracked by PRAGMA user_version. migrations.sql
// stays the idempotent base schema; anything that is not idempotent goes here.
var versionedMigrations = []string{
	// v1: full-text index over conversation transcripts (goals, user messages, model reasoning,
	// tool calls and observations) for session_search.
	`CREATE VIRTUAL TABLE IF NOT EXISTS transcript_fts USING fts5(
		text,
		run_id UNINDEXED,
		branch_id UNINDEXED,
		step_id UNINDEXED,
		kind UNINDEXED,
		trust UNINDEXED,
		created_at UNINDEXED,
		tokenize = 'porter unicode61'
	)`,
	// v2: per-step workspace checkpoints (content-addressed manifests in the blob store).
	`CREATE TABLE IF NOT EXISTS workspace_checkpoints (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		branch_id TEXT NOT NULL,
		step_id TEXT NOT NULL DEFAULT '',
		manifest_hash TEXT NOT NULL,
		files INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_workspace_checkpoints_branch_step ON workspace_checkpoints(branch_id, step_id);
	CREATE INDEX IF NOT EXISTS idx_workspace_checkpoints_run ON workspace_checkpoints(run_id);`,
}

// SchemaVersion is the number of versioned migrations this binary knows.
func SchemaVersion() int { return len(versionedMigrations) }

func applyVersionedMigrations(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(versionedMigrations) {
		return fmt.Errorf("database schema version %d is newer than this soma binary supports (%d); upgrade soma", current, len(versionedMigrations))
	}
	for v := current; v < len(versionedMigrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(versionedMigrations[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("schema migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// TranscriptEntry is one searchable piece of a conversation.
type TranscriptEntry struct {
	RunID    string
	BranchID string
	StepID   string
	Kind     string // goal | user | assistant | tool | observation
	Trust    string // trusted_repo | untrusted_external
	Text     string
}

const maxIndexedTextBytes = 8000

// IndexTranscript adds entries to the full-text transcript index.
func (s *Store) IndexTranscript(ctx context.Context, entries []TranscriptEntry) error {
	if len(entries) == 0 {
		return nil
	}
	now := time.Now().Unix()
	return s.WithWriteTx(ctx, func(conn *sql.Conn) error {
		for _, e := range entries {
			text := strings.TrimSpace(e.Text)
			if text == "" {
				continue
			}
			if len(text) > maxIndexedTextBytes {
				text = text[:maxIndexedTextBytes]
			}
			trust := e.Trust
			if trust == "" {
				trust = "trusted_repo"
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO transcript_fts(text, run_id, branch_id, step_id, kind, trust, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				text, e.RunID, e.BranchID, e.StepID, e.Kind, trust, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// TranscriptHit is a ranked full-text match.
type TranscriptHit struct {
	RunID     string
	BranchID  string
	StepID    string
	Kind      string
	Trust     string
	Snippet   string
	Score     float64
	CreatedAt int64
}

// SearchTranscripts ranks transcript entries matching query with BM25. All terms must match;
// if nothing does, any term may match. Query syntax is never passed through to FTS5.
func (s *Store) SearchTranscripts(ctx context.Context, query string, limit int) ([]TranscriptHit, error) {
	terms := ftsTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	hits, err := s.searchFTS(ctx, strings.Join(terms, " AND "), limit)
	if err != nil || len(hits) > 0 || len(terms) == 1 {
		return hits, err
	}
	return s.searchFTS(ctx, strings.Join(terms, " OR "), limit)
}

func (s *Store) searchFTS(ctx context.Context, match string, limit int) ([]TranscriptHit, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT run_id, branch_id, step_id, kind, trust,
		       snippet(transcript_fts, 0, '[', ']', ' … ', 24), bm25(transcript_fts), created_at
		FROM transcript_fts WHERE transcript_fts MATCH ?
		ORDER BY bm25(transcript_fts) LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TranscriptHit
	for rows.Next() {
		var h TranscriptHit
		if err := rows.Scan(&h.RunID, &h.BranchID, &h.StepID, &h.Kind, &h.Trust, &h.Snippet, &h.Score, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsTerms turns free text into quoted FTS5 terms (letters/digits only), so user input can
// never inject FTS5 operators or column filters.
func ftsTerms(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if len(out) >= 16 {
			break
		}
		out = append(out, `"`+f+`"`)
	}
	return out
}
