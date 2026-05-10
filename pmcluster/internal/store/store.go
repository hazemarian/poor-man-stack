// Package store wraps a SQLite (modernc.org/sqlite, pure Go) database used
// for pmcluster's persistent state: users, managed credentials, stack
// revisions, registry credentials, webhook secrets, backup metadata.
//
// The schema is built up phase-by-phase via embedded migrations under
// pmcluster/migrations. Phase 1 migrations cover only the schema_version
// bookkeeping table and the users table.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	// modernc.org/sqlite is registered as the "sqlite" driver via blank import.
	// It's pure Go (no cgo), which keeps cross-compilation trivial.
	_ "modernc.org/sqlite"
)

// Store is a thin wrapper around *sql.DB that runs migrations on Open and
// hosts per-resource methods grouped in sibling files (users.go in Phase 1.5,
// stacks.go in Phase 3, etc.).
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath, applies any
// outstanding migrations, and returns a ready-to-use *Store. Parent
// directories are created with mode 0700 if missing — pmcluster owns the
// directory exclusively.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("ensure data dir: %w", err)
	}
	// _journal_mode=WAL gives concurrent reads alongside one writer; sufficient
	// for our workload (CLI + single-process daemon).
	// _busy_timeout=5000 backs off briefly when the writer is busy rather than
	// failing immediately with SQLITE_BUSY.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Limit to a single writer connection to avoid SQLITE_BUSY races between
	// goroutines holding their own connections; reads are still concurrent
	// thanks to WAL mode.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the underlying database handle. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB. Reserved for tests and migrations
// inspection — production code should use the typed methods on Store.
func (s *Store) DB() *sql.DB { return s.db }
