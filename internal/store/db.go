package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

//go:embed migrations.sql
var migrationsSQL string

// Store owns split reader/writer SQLite WAL pools and the content-addressed blob store.
type Store struct {
	somaDir string
	dbPath  string
	writer  *sql.DB
	reader  *sql.DB
	blobs   *BlobStore
}

// Open initializes the .soma directory, SQLite WAL pools, schema migrations, and blob store.
func Open(somaDir string) (*Store, error) {
	absDir, err := filepath.Abs(somaDir)
	if err != nil {
		return nil, fmt.Errorf("resolve somaDir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create somaDir: %w", err)
	}

	blobs, err := NewBlobStore(absDir)
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(absDir, "project.db")
	writerDSN := buildSQLiteDSN(dbPath, false)
	writer, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		_ = blobs.Close()
		return nil, fmt.Errorf("open writer sqlite: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)

	if err := writer.Ping(); err != nil {
		_ = writer.Close()
		_ = blobs.Close()
		return nil, fmt.Errorf("ping writer sqlite: %w", err)
	}
	if _, err := writer.Exec(migrationsSQL); err != nil {
		_ = writer.Close()
		_ = blobs.Close()
		return nil, fmt.Errorf("apply schema migrations: %w", err)
	}

	if err := ensureColumn(writer, "accounts", "spent_day", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = writer.Close()
		_ = blobs.Close()
		return nil, fmt.Errorf("apply schema upgrades: %w", err)
	}
	if err := applyVersionedMigrations(writer); err != nil {
		_ = writer.Close()
		_ = blobs.Close()
		return nil, err
	}

	readerDSN := buildSQLiteDSN(dbPath, true)
	reader, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		_ = writer.Close()
		_ = blobs.Close()
		return nil, fmt.Errorf("open reader sqlite: %w", err)
	}
	readerConns := runtime.NumCPU()
	if readerConns < 4 {
		readerConns = 4
	}
	reader.SetMaxOpenConns(readerConns)
	reader.SetMaxIdleConns(readerConns)

	if err := reader.Ping(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = blobs.Close()
		return nil, fmt.Errorf("ping reader sqlite: %w", err)
	}

	return &Store{
		somaDir: absDir,
		dbPath:  dbPath,
		writer:  writer,
		reader:  reader,
		blobs:   blobs,
	}, nil
}

// ensureColumn adds a column to an existing table when an older database predates it.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}

func buildSQLiteDSN(dbPath string, readOnly bool) string {
	u := url.URL{Scheme: "file", Path: dbPath}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
	} else {
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	return u.String() + "?" + q.Encode()
}

// Close closes the reader/writer database pools and the blob store.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	if s.blobs != nil {
		errs = append(errs, s.blobs.Close())
	}
	return errors.Join(errs...)
}

// Blobs returns the underlying content-addressed blob store.
func (s *Store) Blobs() *BlobStore {
	return s.blobs
}

// SomaDir returns the absolute path to the .soma directory.
func (s *Store) SomaDir() string {
	return s.somaDir
}

// DBPath returns the absolute path to project.db.
func (s *Store) DBPath() string {
	return s.dbPath
}

// DBSizeBytes returns the sum of project.db and its WAL/SHM files if present.
func (s *Store) DBSizeBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(s.dbPath + suffix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// WithWriteTx acquires the single writer connection and runs fn inside BEGIN IMMEDIATE.
func (s *Store) WithWriteTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire writer conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return fmt.Errorf("commit immediate tx: %w", err)
	}
	return nil
}
