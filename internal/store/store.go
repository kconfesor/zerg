// Package store owns every byte of zerg's persistent state.
//
// One SQLite file holds both the global role library and per-project runtime.
// The predecessor spread the same information across config files, prompt
// files, and per-worktree snapshots of both, which is how a config edit could
// silently reach nobody. There is exactly one source of truth here.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is compared against SQLite's user_version pragma. Bump it and
// add a migration when the schema changes.
const schemaVersion = 1

// DB is the handle every other package takes.
type DB struct {
	sql *sql.DB
}

// DefaultPath is ~/.zerg/zerg.db — the library is global, so the database is
// too. Per-project state is keyed by project_id inside it.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".zerg", "zerg.db"), nil
}

// Open opens (creating if needed) the database at path and migrates it.
// Pass ":memory:" for tests.
func Open(ctx context.Context, path string) (*DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
	}

	// WAL gives concurrent readers alongside the single writer; busy_timeout
	// turns a momentary lock into a wait rather than an error; foreign_keys
	// makes the ON DELETE CASCADE on project_roles actually fire.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// One writer. SQLite serialises writes anyway; capping the pool means we
	// wait in Go rather than collecting SQLITE_BUSY from the driver.
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sql: sqlDB}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// SQL exposes the handle for packages that need their own queries.
func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) migrate(ctx context.Context) error {
	var version int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	if version > schemaVersion {
		return fmt.Errorf("database schema is version %d, this build understands %d — upgrade zerg",
			version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration: %w", err)
	}
	defer tx.Rollback()

	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
	}

	// PRAGMA does not accept a bind parameter.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("recording schema version: %w", err)
	}
	return tx.Commit()
}
