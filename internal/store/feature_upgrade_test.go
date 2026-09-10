package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestFeatureAccountingUpgradePreservesExistingWork(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "before-feature-accounting.db")
	child, project, _ := seedAtVersion(t, path, 42)
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tasks (id, project_id, name, body, kind, lane, state, created_at) VALUES ('feature', ?, 'Feature', 'original brief', 'feature', '', 'working', '2026-07-01')`, []any{project}},
		{`UPDATE tasks SET parent_id = 'feature' WHERE id = ?`, []any{child}},
		{`INSERT INTO usage_turns (id, project_id, task_id, role, ts, cost_usd) VALUES ('usage', ?, ?, 'coder', '2026-07-01', 3)`, []any{project, child}},
		{`INSERT INTO messages (id, project_id, task_id, from_role, kind, body, created_at) VALUES ('message', ?, ?, 'coder', 'note', 'retained transcript', '2026-07-01')`, []any{project, child}},
		{`INSERT INTO events (id, project_id, task_id, role, kind, text, ts) VALUES ('event', ?, ?, 'coder', 'message', 'retained event', '2026-07-01')`, []any{project, child}},
		{`INSERT INTO clarifications (id, project_id, task_id, role, question, state, created_at) VALUES ('question', ?, ?, 'coder', 'retained question', 'open', '2026-07-01')`, []any{project, child}},
	} {
		if _, err := raw.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"usage_turns", "messages", "events", "clarifications"} {
		var count int
		if err := db.Read().QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE task_id = ?`, child).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s rows = %d: %v", table, count, err)
		}
	}
	var owner string
	if err := db.Read().QueryRowContext(ctx, `SELECT feature_id FROM usage_turns WHERE id = 'usage'`).Scan(&owner); err != nil || owner != "feature" {
		t.Fatalf("usage ownership was not backfilled: %q %v", owner, err)
	}
	total, err := db.UsageForTask(ctx, "feature")
	if err != nil || total.CostUSD != 3 {
		t.Fatalf("feature cost = %+v %v", total, err)
	}
	var violations int
	rows, err := db.Read().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		violations++
	}
	if err := rows.Err(); err != nil || violations != 0 {
		t.Fatalf("foreign key violations = %d: %v", violations, err)
	}
}
