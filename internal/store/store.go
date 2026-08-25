// Package store owns every byte of zerg's persistent state.
//
// One SQLite file holds both the global role library and per-project runtime.
// Spreading the same information across config files, prompt files, and
// per-worktree snapshots of both is how a config edit silently reaches nobody.
// There is exactly one source of truth here.
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

//go:embed schema_001.sql
var schema001 string

//go:embed schema_002.sql
var schema002 string

//go:embed schema_003.sql
var schema003 string

//go:embed schema_004.sql
var schema004 string

//go:embed schema_005.sql
var schema005 string

// migrations are applied in order; a database at user_version N has had the
// first N of them run. To change the schema, append a file and a line here —
// never edit one that has shipped.
var migrations = []string{schema001, schema002, schema003, schema004, schema005}

func schemaVersion() int { return len(migrations) }

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

	target := schemaVersion()
	if version > target {
		return fmt.Errorf("database schema is version %d, this build understands %d — upgrade zerg",
			version, target)
	}
	if version == target {
		return nil
	}

	// All outstanding migrations in one transaction: a half-migrated database
	// is worse than an unmigrated one.
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration: %w", err)
	}
	defer tx.Rollback()

	for i := version; i < target; i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("applying migration %d: %w", i+1, err)
		}
	}

	// PRAGMA does not accept a bind parameter.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
		return fmt.Errorf("recording schema version: %w", err)
	}
	return tx.Commit()
}
