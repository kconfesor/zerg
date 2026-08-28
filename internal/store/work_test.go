package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A stopped card must actually stop. Setting the state alone leaves its route
// queued, so an agent claims it two seconds later and the board shows a card
// working that a person just stopped.
func TestStoppingACardClosesItsRoutesAndLease(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Stoppable", "body", "coder")
	if err != nil {
		t.Fatal(err)
	}
	// A message routed to the role, as a real card has.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, priority, body, created_at)
		 VALUES ('m1', ?, ?, 'operator', 'note', 50, 'go', datetime('now'))`,
		p.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO routes (message_id, to_role, state, enqueued_at)
		 VALUES ('m1','coder','queued', datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	if err := db.StopTask(ctx, p.ID, task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TaskRejected {
		t.Errorf("state = %q, want rejected", got.State)
	}

	// The part that matters: nothing is left claimable.
	n, err := db.QueuedCount(ctx, p.ID, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d route(s) still queued after stopping, so an agent will pick it up again", n)
	}
}

// Deleting a card takes its transcript, which is only readable beside it, and
// leaves the spend, which was real.
func TestDeletingACardKeepsWhatWasActuallySpent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Doomed", "body", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUsage(ctx, UsageTurn{
		ProjectID: p.ID, TaskID: &task.ID, Role: "coder",
		Model: "m", Provider: "x", OutputTokens: 100, CostUSD: 1.25,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO events (id, project_id, task_id, role, kind, ts, text)
		 VALUES ('e1', ?, ?, 'coder', 'message', datetime('now'), 'hello')`,
		p.ID, task.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteTask(ctx, p.ID, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	var events int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE id = 'e1'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Error("the transcript survived its card and is now unreachable")
	}

	// The bill does not change because a card was tidied away.
	rows, err := db.UsageByGroup(ctx, p.ID, "role", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	for _, r := range rows {
		cost += r.CostUSD
	}
	if cost != 1.25 {
		t.Errorf("project cost is %.2f after deleting a card, want 1.25 because the money was spent", cost)
	}
}

// A deleted card takes its questions with it.
//
// clarifications.task_id is ON DELETE SET NULL, so the delete used to detach an
// open question rather than remove it: an item in Attention naming a card that
// no longer exists, which a person cannot clear because answering is the only
// thing they can do to one.
func TestDeletingACardRemovesItsQuestions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Graph function", "do it", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AskClarification(ctx, p.ID, "coder", "which range?", &task.ID); err != nil {
		t.Fatal(err)
	}
	// One asked about no card at all, which `zerg ask` allows and which must
	// survive: it is not this card's, so it is not this card's to take.
	if _, err := db.AskClarification(ctx, p.ID, "coder", "unrelated", nil); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteTask(ctx, p.ID, task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	left, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("%d open question(s) after deleting the card, want 1", len(left))
	}
	if left[0].Question != "unrelated" {
		t.Errorf("survivor is %q, want the one that was never about this card", left[0].Question)
	}
}

// Stopping is the same class of event by a different route: the card is over,
// so the question the role was waiting on cannot be answered usefully either.
func TestStoppingACardCancelsItsQuestions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Graph function", "do it", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AskClarification(ctx, p.ID, "coder", "which range?", &task.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.StopTask(ctx, p.ID, task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	left, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d question(s) still open on a stopped card", len(left))
	}
}

// Migration 19 recovers the one outcome the old database wrote down.
//
// A pull request URL was appended to the terminal handoff's note as prose, so
// it is the only ending that can be read back with confidence. A merge and a
// branch left the same trace, which is none, and those stay empty rather than
// being guessed from a project setting that may have changed since.
func TestMigrationReadsBackThePullRequestItWroteIntoProse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prose.db")
	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 18; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version=18`); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (id,path,name,base_branch,created_at)
		VALUES ('p',?,'Old','main',?)`, repoDir(t, "prose"), now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, body string }{
		{"shipped", "did the thing\n\nPull request: https://example.test/pr/7"},
		{"merged", "did the thing"},
	} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO tasks (id,project_id,name,body,lane,state,created_at,completed_at)
			VALUES (?,'p',?,'','done','done',?,?)`, tc.id, tc.id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at)
			VALUES (?,'p',?,'reviewer','handoff',50,?,1,?)`, "m-"+tc.id, tc.id, tc.body, now); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating: %v", err)
	}
	defer db.Close()

	shipped, err := db.GetTask(ctx, "shipped")
	if err != nil {
		t.Fatal(err)
	}
	if shipped.Outcome != OutcomePR || shipped.OutcomeRef != "https://example.test/pr/7" {
		t.Errorf("recovered %q %q, want the pull request it named", shipped.Outcome, shipped.OutcomeRef)
	}

	// The other one is not guessed. A merge and a branch are indistinguishable
	// in an old database, and saying either would be making it up.
	merged, err := db.GetTask(ctx, "merged")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Outcome != "" {
		t.Errorf("an ending nothing recorded came back as %q", merged.Outcome)
	}
}
