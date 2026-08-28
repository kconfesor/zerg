package store

import (
	"context"
	"database/sql"
	"fmt"
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

// History is the list of what was worked on, newest first, a page at a time.
func TestListHistoryPagesAndFilters(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	// Six cards, each finished a minute after the last, so the order is known.
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		name     string
		outcome  string
		role     string
		finished bool
	}{
		{"Factorial", OutcomeMerged, "coder", true},
		{"Power", OutcomePR, "reviewer", true},
		{"Readme", OutcomeBranch, "docs", true},
		{"Percent", OutcomeMerged, "coder", true},
		{"Quota probe", "", "coder", true},
		{"Variables", "", "coder", false}, // still running
	} {
		task, err := db.CreateTask(ctx, p.ID, tc.name, "body", "")
		if err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at)
			 VALUES (?,?,?,?,'handoff',50,'',0,?)`,
			NewID(), p.ID, task.ID, tc.role, at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO usage_turns (id,project_id,task_id,role,ts,output_tokens,cost_usd)
			 VALUES (?,?,?,?,?,?,?)`,
			NewID(), p.ID, task.ID, tc.role, at, 1_500_000, 2.5); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO events (id,project_id,task_id,role,kind,ts,text,tool,fatal)
			 VALUES (?,?,?,?,'message',?,'did it','',0)`,
			NewID(), p.ID, task.ID, tc.role, at); err != nil {
			t.Fatal(err)
		}
		if tc.finished {
			if _, err := db.SQL().ExecContext(ctx,
				`UPDATE tasks SET state='done', lane='done', completed_at=?, outcome=?, created_at=? WHERE id=?`,
				at, tc.outcome, at, task.ID); err != nil {
				t.Fatal(err)
			}
		} else if _, err := db.SQL().ExecContext(ctx,
			`UPDATE tasks SET created_at=? WHERE id=?`, at, task.ID); err != nil {
			t.Fatal(err)
		}
	}

	names := func(entries []HistoryEntry) []string {
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}

	// Newest first, and a card still running is part of history: it is being
	// worked on, which is what the list is about.
	page, next, err := db.ListHistory(ctx, p.ID, HistoryFilter{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page); fmt.Sprint(got) != "[Variables Quota probe Percent Readme]" {
		t.Errorf("first page is %v", got)
	}
	if next == "" {
		t.Fatal("no cursor, so the older work cannot be reached")
	}

	// The cursor is a position rather than an offset, so the second page picks
	// up exactly where the first stopped, with nothing repeated or skipped.
	rest, last, err := db.ListHistory(ctx, p.ID, HistoryFilter{Limit: 4, Before: next})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(rest); fmt.Sprint(got) != "[Power Factorial]" {
		t.Errorf("second page is %v", got)
	}
	if last != "" {
		t.Errorf("cursor %q on the last page, which has nothing after it", last)
	}

	// What each card cost and who touched it come back with it. Asserted with
	// real numbers rather than zeros: every column of this row is read
	// positionally, and a fixture of zeros converts into whatever type the scan
	// happens to offer. Adding a column put tokens where a boolean was expected
	// and this test stayed green while the endpoint returned 500.
	if page[3].Tokens != 1_500_000 || page[3].CostUSD != 2.5 {
		t.Errorf("Readme came back with %d tokens and %v, want what it spent", page[3].Tokens, page[3].CostUSD)
	}
	if !page[3].HasTranscript {
		t.Error("Readme has events, so its transcript is still readable")
	}
	if len(page[3].Roles) != 1 || page[3].Roles[0] != "docs" {
		t.Errorf("Readme was worked by %v, want docs", page[3].Roles)
	}

	for _, tc := range []struct {
		what   string
		filter HistoryFilter
		want   string
	}{
		{"outcome", HistoryFilter{Outcome: OutcomeMerged}, "[Percent Factorial]"},
		{"no outcome", HistoryFilter{Outcome: "none"}, "[Variables Quota probe]"},
		{"role", HistoryFilter{Role: "reviewer"}, "[Power]"},
		{"name", HistoryFilter{Query: "act"}, "[Factorial]"},
		{"nothing matches", HistoryFilter{Query: "nothing here"}, "[]"},
	} {
		got, _, err := db.ListHistory(ctx, p.ID, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if n := names(got); fmt.Sprint(n) != tc.want && !(len(got) == 0 && tc.want == "[]") {
			t.Errorf("filter by %s gave %v, want %s", tc.what, n, tc.want)
		}
	}

	// A search for a wildcard is a search for that character, not for
	// everything: "%" typed into the box used to return the whole list.
	if _, err := db.CreateTask(ctx, p.ID, "100% coverage", "body", ""); err != nil {
		t.Fatal(err)
	}
	hits, _, err := db.ListHistory(ctx, p.ID, HistoryFilter{Query: "100%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("searching for a literal %% matched %d cards, want the one named that", len(hits))
	}
}

// The trail is the handoffs with the numbers that belong to each: how long the
// role held the work, what those turns cost, what stopped it there.
//
// Per step rather than per task, because "this pipeline cost four dollars" does
// not say which role spent it, and a card whose wall time is hours and whose
// active time is minutes has that gap somewhere specific.
func TestTaskTrailCarriesEachStepsTimeAndCost(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Factorial", "body", "")
	if err != nil {
		t.Fatal(err)
	}

	at := func(min int) string {
		return time.Date(2026, 1, 1, 9, min, 0, 0, time.UTC).Format(time.RFC3339Nano)
	}
	// The coder holds it for ten minutes and hands off, having spent two turns
	// and asked one question along the way.
	lease := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO leases (id,project_id,role,granted_at,expires_at) VALUES (?,?,'coder',?,?)`,
		lease, p.ID, at(0), at(30)); err != nil {
		t.Fatal(err)
	}
	handoff := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at,source_lease_id)
		 VALUES (?,?,?,'coder','handoff',50,'did it',0,?,?)`,
		handoff, p.ID, task.ID, at(10), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO routes (message_id,to_role,state,enqueued_at) VALUES (?,'reviewer','queued',?)`,
		handoff, at(10)); err != nil {
		t.Fatal(err)
	}
	for i, turn := range []struct {
		role   string
		minute int
		cost   float64
	}{
		{"coder", 3, 0.10},
		// The turn that ends a step lands after the handoff it produced: the
		// agent calls `zerg send`, the message is written, and only then does
		// the turn carrying that call finish. Closing the window at the handoff
		// dropped the largest turn of every step.
		{"coder", 11, 0.15},
		{"coder", 40, 0.99}, // a second lap, which its own lease claims below
	} {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO usage_turns (id,project_id,task_id,role,ts,output_tokens,cost_usd)
			 VALUES (?,?,?,?,?,?,?)`,
			NewID(), p.ID, task.ID, turn.role, at(turn.minute), 100*(i+1), turn.cost); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO clarifications (id,project_id,task_id,role,question,state,created_at)
		 VALUES (?,?,?,'coder','which base?','open',?)`,
		NewID(), p.ID, task.ID, at(5)); err != nil {
		t.Fatal(err)
	}

	// The coder gets the work back and takes another lap. Its first window
	// closes here and not before, which is what keeps the two laps apart.
	secondLease := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO leases (id,project_id,role,granted_at,expires_at) VALUES (?,?,'coder',?,?)`,
		secondLease, p.ID, at(38), at(60)); err != nil {
		t.Fatal(err)
	}
	lap := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at,source_lease_id)
		 VALUES (?,?,?,'coder','handoff',50,'fixed it',0,?,?)`,
		lap, p.ID, task.ID, at(42), secondLease); err != nil {
		t.Fatal(err)
	}

	// The reviewer finishes it behind a gate that a person took an hour to
	// answer: the wait is where this card's wall time went.
	reviewLease := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO leases (id,project_id,role,granted_at,expires_at) VALUES (?,?,'reviewer',?,?)`,
		reviewLease, p.ID, at(45), at(90)); err != nil {
		t.Fatal(err)
	}
	final := NewID()
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at,source_lease_id)
		 VALUES (?,?,?,'reviewer','handoff',50,'approved',1,?,?)`,
		final, p.ID, task.ID, at(50), reviewLease); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO approvals (id,project_id,message_id,state,created_at,decided_at)
		 VALUES (?,?,?,'approved',?,?)`,
		NewID(), p.ID, final, at(50), at(110)); err != nil {
		t.Fatal(err)
	}

	trail, err := db.TaskTrail(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskTrail: %v", err)
	}
	if len(trail) != 3 {
		t.Fatalf("trail has %d steps, want 3: %+v", len(trail), trail)
	}

	coder := trail[0]
	if coder.From != "coder" || coder.To != "reviewer" {
		t.Errorf("first step is %s to %s", coder.From, coder.To)
	}
	if coder.DurationMS != 10*60_000 {
		t.Errorf("the coder held it for %dms, want ten minutes", coder.DurationMS)
	}
	// The turns of that lap, including the one recorded just after the handoff,
	// and not the one from the lap the coder took later.
	if coder.CostUSD < 0.249 || coder.CostUSD > 0.251 {
		t.Errorf("the first lap cost %v, want the two turns it spent", coder.CostUSD)
	}
	if coder.Tokens != 300 {
		t.Errorf("the first lap used %d tokens, want 300", coder.Tokens)
	}
	if len(coder.Clarifications) != 1 || coder.Clarifications[0].Question != "which base?" {
		t.Errorf("the question asked mid-step is not on it: %+v", coder.Clarifications)
	}
	if coder.Gate != nil {
		t.Errorf("an ungated handoff came back with a gate: %+v", coder.Gate)
	}

	// The second lap is its own step, with its own spend. Two visits by one
	// role are the thing rework is, and a total that merged them would hide it.
	lapStep := trail[1]
	if lapStep.From != "coder" || lapStep.CostUSD < 0.98 || lapStep.CostUSD > 1.0 {
		t.Errorf("the second lap is %s costing %v, want the coder spending 0.99", lapStep.From, lapStep.CostUSD)
	}

	reviewer := trail[2]
	if !reviewer.Final || reviewer.To != "" {
		t.Errorf("the last step should finish the task: %+v", reviewer.Handoff)
	}
	if reviewer.Gate == nil {
		t.Fatal("the gated handoff has no gate")
	}
	// An hour of a card's life spent waiting for a person, which no per-task
	// total distinguishes from an hour of work.
	if reviewer.Gate.WaitedMS != 60*60_000 {
		t.Errorf("the gate waited %dms, want an hour", reviewer.Gate.WaitedMS)
	}
	if reviewer.Gate.State != "approved" {
		t.Errorf("gate state is %q", reviewer.Gate.State)
	}
}

// The board's own query, with a card that has spent something.
//
// It reads the task columns positionally and adds three of its own, so a column
// added to the list without a destination in the scanner takes the board down
// with "expected 20 destination arguments in Scan, not 19" and nothing else
// notices. Twice now: once for the outcome, once for the pin.
func TestListTasksReadsEveryColumnItSelects(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	task, err := db.CreateTask(ctx, p.ID, "Factorial", "body", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskPinned(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO usage_turns (id,project_id,task_id,role,ts,output_tokens,cost_usd)
		 VALUES (?,?,?,'coder',?,?,?)`,
		NewID(), p.ID, task.ID, time.Now().UTC().Format(time.RFC3339Nano), 4_000, 1.25); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, &Event{
		ID: NewID(), ProjectID: p.ID, TaskID: &task.ID,
		Role: "coder", Kind: "tool_call", At: time.Now().UTC(), Tool: "Bash",
	}); err != nil {
		t.Fatal(err)
	}

	tasks, err := db.ListTasks(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d cards, want 1", len(tasks))
	}
	got := tasks[0]
	if !got.Pinned {
		t.Error("the board lost the pin")
	}
	if got.Tokens != 4_000 || got.CostUSD != 1.25 {
		t.Errorf("the board reports %d tokens and %v, want what the card spent", got.Tokens, got.CostUSD)
	}
	if got.Doing != "Bash" {
		t.Errorf("the board reports %q as the last thing done, want the tool", got.Doing)
	}
}
