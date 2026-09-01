// Package store owns the SQLite database: connection setup, schema
// migrations, and typed access to the tables.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the binary cgo-free
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Common store errors. Handlers map these onto HTTP status codes.
var (
	ErrNotFound  = errors.New("not found")
	ErrNameTaken = errors.New("name already in use")
)

// DB wraps the SQLite handle.
type DB struct {
	sql *sql.DB
}

// Open connects to the database at path, applies pragmas, and runs any
// outstanding migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	// _txlock=immediate avoids SQLITE_BUSY upgrade deadlocks between
	// concurrent write transactions.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", filepath.Clean(path))
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	// SQLite writes serialise anyway; a small pool keeps lock contention
	// predictable rather than surfacing as busy timeouts.
	sqlDB.SetMaxOpenConns(4)

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connecting to database %s: %w", path, err)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// migrate applies embedded migration files in filename order, exactly once
// each, recording what has been applied in schema_migrations.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("listing migrations: %w", err)
	}
	sort.Strings(entries)

	for _, entry := range entries {
		name := filepath.Base(entry)

		var applied int
		if err := d.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("checking migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}

		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("starting migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", name, err)
		}
	}
	return nil
}

// newID returns a random identifier. Target IDs become path segments under
// the mount root, so the alphabet is restricted to hex.
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
