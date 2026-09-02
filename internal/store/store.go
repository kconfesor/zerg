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

//go:embed schema_006.sql
var schema006 string

//go:embed schema_007.sql
var schema007 string

//go:embed schema_008.sql
var schema008 string

//go:embed schema_009.sql
var schema009 string

//go:embed schema_010.sql
var schema010 string

//go:embed schema_011.sql
var schema011 string

//go:embed schema_012.sql
var schema012 string

//go:embed schema_013.sql
var schema013 string

//go:embed schema_014.sql
var schema014 string

//go:embed schema_015.sql
var schema015 string

//go:embed schema_016.sql
var schema016 string

//go:embed schema_017.sql
var schema017 string

//go:embed schema_018.sql
var schema018 string

//go:embed schema_019.sql
var schema019 string

//go:embed schema_020.sql
var schema020 string

//go:embed schema_021.sql
var schema021 string

//go:embed schema_022.sql
var schema022 string

//go:embed schema_023.sql
var schema023 string

//go:embed schema_024.sql
var schema024 string

//go:embed schema_025.sql
var schema025 string

//go:embed schema_026.sql
var schema026 string

//go:embed schema_027.sql
var schema027 string

//go:embed schema_028.sql
var schema028 string

//go:embed schema_029.sql
var schema029 string

//go:embed schema_030.sql
var schema030 string

//go:embed schema_031.sql
var schema031 string

//go:embed schema_032.sql
var schema032 string

//go:embed schema_033.sql
var schema033 string

//go:embed schema_034.sql
var schema034 string

//go:embed schema_035.sql
var schema035 string

//go:embed schema_036.sql
var schema036 string

//go:embed schema_037.sql
var schema037 string

// migrations are applied in order; a database at user_version N has had the
// first N of them run. To change the schema, append a file and a line here —
// never edit one that has shipped.
var migrations = []string{schema001, schema002, schema003, schema004, schema005, schema006, schema007, schema008, schema009, schema010, schema011, schema012, schema013, schema014, schema015, schema016, schema017, schema018, schema019, schema020, schema021, schema022, schema023, schema024, schema025, schema026, schema027, schema028, schema029, schema030, schema031, schema032, schema033, schema034, schema035, schema036, schema037}

func schemaVersion() int { return len(migrations) }

// DB is the handle every other package takes.
//
// Two pools over one file, not one. SQLite serialises writers, so the write
// pool is capped at a single connection and the wait happens in Go rather than
// as SQLITE_BUSY from the driver. Capping *everything* at one connection also
// serialised every read behind every write, which threw away the one thing WAL
// is for — readers that do not block on the writer — and made a slow commit
// stall the board poll, the activity replay and the recorder's own lookups
// behind it.
//
// The read pool is opened query_only, so a read path that reaches for the
// wrong handle fails loudly instead of quietly becoming a second writer.
type DB struct {
	sql  *sql.DB
	read *sql.DB
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
		// 0700: the database holds every prompt, transcript and cost this
		// machine has produced, and nothing else on it has a reason to read them.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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

	db := &DB{sql: sqlDB, read: sqlDB}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}

	// A second pool for reads, where there is a file to open twice.
	//
	// ":memory:" is deliberately excluded: each connection to it is its own
	// empty database, so a second handle would be a second, empty store rather
	// than another view of this one. Tests run single-pool and lose nothing but
	// the concurrency they were not exercising.
	if path != ":memory:" {
		readDB, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
		if err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("opening %s for reading: %w", path, err)
		}
		readDB.SetMaxOpenConns(readers)
		if err := readDB.PingContext(ctx); err != nil {
			readDB.Close()
			sqlDB.Close()
			return nil, fmt.Errorf("opening %s for reading: %w", path, err)
		}
		db.read = readDB
	}

	if path != ":memory:" {
		tighten(path)
	}
	return db, nil
}

// readers is the read pool's size. Small on purpose: the concurrent readers
// here are a handful of HTTP handlers and the recorder, and every one of them
// holds a file descriptor and a page cache.
const readers = 8

// tighten narrows an existing installation's file modes.
//
// MkdirAll and OpenFile apply their mode only when they create something, so a
// database created before these modes were chosen keeps whatever it was made
// with — 0755 and 0644 under the usual umask, which is world-readable. This
// file holds every prompt, transcript and cost on the machine, and the -wal
// sidecar holds recently written copies of the same rows.
// Best effort, every one of them. A chmod that fails is a mode this process
// could not set — on a directory it does not own, on a filesystem without unix
// permissions, on a database somebody deliberately put somewhere shared. None
// of those is a reason to refuse to run: the file is already there with the
// mode it already has, so failing the open protects nothing and costs the
// operator their daemon. Pointing --db at /tmp did exactly that.
func tighten(path string) {
	_ = os.Chmod(filepath.Dir(path), 0o700)
	// The sidecars are created by SQLite, not by us, and carry the same rows.
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		_ = os.Chmod(p, 0o600)
	}
}

func (db *DB) Close() error {
	if db.read != nil && db.read != db.sql {
		db.read.Close()
	}
	return db.sql.Close()
}

// SQL exposes the write handle for packages that need their own transactions.
func (db *DB) SQL() *sql.DB { return db.sql }

// Read exposes the read pool, for queries that take no write lock.
func (db *DB) Read() *sql.DB { return db.read }

func (db *DB) migrate(ctx context.Context) error {
	var version int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	target := schemaVersion()
	if version > target {
		return fmt.Errorf("database schema is version %d, this build understands %d, so upgrade zerg",
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
