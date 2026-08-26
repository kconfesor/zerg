package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// A database left by the previous release must migrate, not fail.
//
// Every other test starts at the current schema, so nothing exercised the one
// path every existing installation takes. The old database is built by applying
// the migrations that had shipped and stopping — rather than by opening at the
// current version and undoing the newest one, which quietly needs editing every
// time a migration is added and fails loudly when someone forgets.
func TestUpgradeFromThePreviousVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	previous := schemaVersion() - 1
	if previous < 1 {
		t.Skip("there is no previous schema version to upgrade from")
	}

	taskID, projectID, otherID := seedAtVersion(t, path, previous)

	// Reopen with this build.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrading a version %d database: %v", previous, err)
	}
	defer db.Close()

	var v int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion() {
		t.Fatalf("user_version is %d after upgrade, want %d", v, schemaVersion())
	}

	// The data is still there, and readable through the current columns.
	task, err := db.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("the existing card did not survive: %v", err)
	}
	if task.ProjectID != projectID {
		t.Errorf("the card came back under project %s, want %s", task.ProjectID, projectID)
	}
	if _, err := db.GetProject(ctx, projectID); err != nil {
		t.Errorf("the existing project did not survive: %v", err)
	}

	// And the integrity check migration 11 added is live on the upgraded file,
	// not only on databases created fresh.
	_, err = db.sql.ExecContext(ctx,
		`INSERT INTO events (id, project_id, task_id, role, kind, ts) VALUES (?,?,?,?,?,?)`,
		NewID(), otherID, taskID, "coder", "message", "2026-01-01T00:00:00Z")
	if err == nil {
		t.Error("an event was stored against another project's task")
	}
}

// seedAtVersion writes a database at the given schema version, with two
// projects and one card, and returns the task, its project, and the other one.
//
// Raw SQL throughout: the store's own methods speak the current schema, and the
// point here is a file that predates it.
func seedAtVersion(t *testing.T, path string, version int) (taskID, projectID, otherID string) {
	t.Helper()
	ctx := context.Background()

	raw, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)

	for i := 0; i < version; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("applying migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	projectID, otherID, taskID = NewID(), NewID(), NewID()
	for _, p := range []struct{ id, path string }{{projectID, t.TempDir()}, {otherID, t.TempDir()}} {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO projects (id, path, name, base_branch, created_at) VALUES (?,?,?,?,?)`,
			p.id, p.path, filepath.Base(p.path), "main", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, body, lane, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		taskID, projectID, "Existing", "from the previous release", "coder", TaskQueued, now); err != nil {
		t.Fatal(err)
	}
	return taskID, projectID, otherID
}
