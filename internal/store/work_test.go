package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
	if _, err := db.AskClarification(ctx, p.ID, "coder", "which range?", nil, &task.ID); err != nil {
		t.Fatal(err)
	}
	// One asked about no card at all, which `zerg ask` allows and which must
	// survive: it is not this card's, so it is not this card's to take.
	if _, err := db.AskClarification(ctx, p.ID, "coder", "unrelated", nil, nil); err != nil {
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
	if _, err := db.AskClarification(ctx, p.ID, "coder", "which range?", nil, &task.ID); err != nil {
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

	// The operator opens every card, so it is on every row and says nothing.
	// Its message is still in the trail; this is the list of agents.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id,project_id,task_id,from_role,kind,priority,body,terminal,created_at)
		 SELECT ?, project_id, id, 'operator', 'handoff', 50, '', 0, created_at FROM tasks WHERE name = 'Factorial'`,
		NewID()); err != nil {
		t.Fatal(err)
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
	oldest, _, err := db.ListHistory(ctx, p.ID, HistoryFilter{Query: "Factorial"})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest) != 1 || len(oldest[0].Roles) != 1 || oldest[0].Roles[0] != "coder" {
		t.Errorf("Factorial was worked by %v, want the agent and not the operator", oldest[0].Roles)
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

// A card that finishes between two pages is still on one of them.
//
// Ordering by "completed, or else created" made a running card's sort key move
// the moment it finished: a card below the cursor jumped above it, and page two
// asks only for what is below, so nothing ever returned it. Unfinished cards
// are their own group at the top now, and a finished card's key never changes.
func TestHistoryPagingDoesNotDropACardThatFinishesMidway(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	ids := map[string]string{}
	for i, name := range []string{"Oldest", "Middle", "Newest"} {
		task, err := db.CreateTask(ctx, p.ID, name, "body", "")
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = task.ID
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		if _, err := db.SQL().ExecContext(ctx,
			`UPDATE tasks SET created_at=?, completed_at=?, state='done', lane='done' WHERE id=?`,
			at, at, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	// One card still being worked on, created before all of them.
	running, err := db.CreateTask(ctx, p.ID, "Running", "body", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE tasks SET created_at=? WHERE id=?`,
		base.Add(-time.Hour).Format(time.RFC3339Nano), running.ID); err != nil {
		t.Fatal(err)
	}

	names := func(entries []HistoryEntry) []string {
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}

	// A card being worked on leads, however old it is: it is the one thing on
	// this list that is still happening.
	page, cursor, err := db.ListHistory(ctx, p.ID, HistoryFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(names(page)); got != "[Running Newest]" {
		t.Fatalf("first page is %s", got)
	}

	// It finishes while the reader is on page one.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE tasks SET state='done', lane='done', completed_at=? WHERE id=?`,
		base.Add(time.Hour).Format(time.RFC3339Nano), running.ID); err != nil {
		t.Fatal(err)
	}

	rest, _, err := db.ListHistory(ctx, p.ID, HistoryFilter{Limit: 5, Before: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(names(rest)); got != "[Middle Oldest]" {
		t.Errorf("second page is %s, want the two below the cursor and nothing repeated", got)
	}
}

// A card says which models did its work, in the order they first spent on it.
//
// What a role is configured with is a live value and answers a different
// question. This one is about the work in front of you: a card that came out
// well or badly was produced by particular models, and after the role's model
// is changed the card must still say what actually made it.
func TestACardReportsTheModelsThatWorkedIt(t *testing.T) {
	ctx := context.Background()
	db, project := seeded(t)

	task, err := db.CreateTask(ctx, project.ID, "Calculator", "", "coder")
	if err != nil {
		t.Fatal(err)
	}

	// planner first, then coder twice on a different model, then the reviewer
	// on a third. Recorded out of order on purpose: the report is by first
	// use, not by insertion.
	for _, u := range []struct {
		role, model string
		at          time.Time
	}{
		{"coder", "claude-sonnet-5", time.Now().Add(-8 * time.Minute)},
		{"planner", "claude-opus-5", time.Now().Add(-10 * time.Minute)},
		{"coder", "claude-sonnet-5", time.Now().Add(-7 * time.Minute)},
		{"reviewer", "gpt-5.6-sol", time.Now().Add(-2 * time.Minute)},
	} {
		if err := db.RecordUsage(ctx, UsageTurn{
			ProjectID: project.ID, TaskID: &task.ID, Role: u.role, Model: u.model,
			Harness: "claude", At: u.at, OutputTokens: 10, CostUSD: 0.01,
		}); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	tasks, err := db.ListTasks(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("%d cards, want 1", len(tasks))
	}
	got := tasks[0].Models
	want := []string{"claude-opus-5", "claude-sonnet-5", "gpt-5.6-sol"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("models = %v, want %v (first use first)", got, want)
			break
		}
	}
}

// Three queries read a card, and each has its own column list and its own
// scanner. A field added to one and forgotten in another is not a compile
// error: it is a card that has a skip list on the board and none in the
// history, which reads as two different cards.
func TestSkipComesBackFromEveryQueryThatReadsACard(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	task, err := db.CreateTask(ctx, p.ID, "Fix a typo", "one line", "coder")
	if err != nil {
		t.Fatal(err)
	}
	// Written straight to the column: nydus owns validating and canonicalising
	// a skip list, and this is about the read paths.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE tasks SET skip = ? WHERE id = ?`, `["role-a","role-b"]`, task.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"role-a", "role-b"}

	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !slices.Equal(got.Skip, want) {
		t.Errorf("GetTask skip = %v, want %v", got.Skip, want)
	}

	board, err := db.ListTasks(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(board) != 1 || !slices.Equal(board[0].Skip, want) {
		t.Errorf("ListTasks skip = %v, want %v", board[0].Skip, want)
	}

	page, _, err := db.ListHistory(ctx, p.ID, HistoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(page) != 1 || !slices.Equal(page[0].Skip, want) {
		t.Errorf("ListHistory skip = %v, want %v", page[0].Skip, want)
	}

	// And a card that skips nothing comes back with nothing, not with an empty
	// list that renders as a badge saying so.
	plain, err := db.CreateTask(ctx, p.ID, "Plain", "body", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := db.GetTask(ctx, plain.ID); err != nil {
		t.Fatal(err)
	} else if again.Skip != nil {
		t.Errorf("skip = %v on a card that skips nothing, want nil", again.Skip)
	}
}

// Options are the agent's own enumeration of the answers, and they have to
// survive the round trip intact: the operator picks one and the answer comes
// back as that text, so an option that is reordered, trimmed away or merged
// with its neighbour is an answer the agent cannot match to what it offered.
func TestAQuestionCarriesTheOptionsItOffered(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	offered := []string{"Redis, shared across instances", "A signed cookie, no server state"}
	asked, err := db.AskClarification(ctx, p.ID, "coder", "Where does the session live?", offered, nil)
	if err != nil {
		t.Fatalf("AskClarification: %v", err)
	}

	if !slices.Equal(asked.Options, offered) {
		t.Errorf("the row it returned has options %q, want %q", asked.Options, offered)
	}

	read, err := db.GetClarification(ctx, asked.ID)
	if err != nil {
		t.Fatalf("GetClarification: %v", err)
	}
	if !slices.Equal(read.Options, offered) {
		t.Errorf("read back options %q, want %q", read.Options, offered)
	}

	open, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 || !slices.Equal(open[0].Options, offered) {
		t.Errorf("Attention sees %+v, want the options the agent offered", open)
	}

	// The answer is one of them verbatim, and stays a plain string: an agent
	// written before options existed reads the same shape it always read.
	if err := db.AnswerClarification(ctx, asked.ID, offered[1]); err != nil {
		t.Fatalf("AnswerClarification: %v", err)
	}
	answered, err := db.GetClarification(ctx, asked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if answered.Answer == nil || *answered.Answer != offered[1] {
		t.Errorf("answer is %v, want the option that was chosen", answered.Answer)
	}
	if !slices.Equal(answered.Options, offered) {
		t.Errorf("answering lost the options: %q", answered.Options)
	}
}

// A question with nothing to choose from is the free-text one this started as,
// and is what every row written before schema 34 reads back as. The column is
// NULL there, not '[]', because it is what tells the cockpit which of the two
// shapes to draw.
func TestAQuestionWithoutOptionsStaysFreeText(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	asked, err := db.AskClarification(ctx, p.ID, "coder", "what is the range?", nil, nil)
	if err != nil {
		t.Fatalf("AskClarification: %v", err)
	}
	if asked.Options != nil {
		t.Errorf("options are %q, want none", asked.Options)
	}

	var stored sql.NullString
	if err := db.sql.QueryRowContext(ctx,
		`SELECT options FROM clarifications WHERE id = ?`, asked.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Errorf("stored options are %q, want NULL", stored.String)
	}

	read, err := db.GetClarification(ctx, asked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Options != nil {
		t.Errorf("read back options %q from a question that offered none", read.Options)
	}
}

// The agent's own mistakes, caught where they can still be reported to it: an
// option that is blank, or the same as another, is a radio button the operator
// cannot tell from its neighbour, and each of these is a 400 naming the fix.
func TestOptionsThatCannotBeChosenBetweenAreRefused(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, maxClarificationOptions+1)
	for i := range many {
		many[i] = fmt.Sprintf("option %d", i)
	}

	for _, c := range []struct {
		name    string
		options []string
	}{
		{"an empty option", []string{"Redis", ""}},
		{"whitespace only", []string{"Redis", "   "}},
		{"the same option twice", []string{"Redis", "Redis"}},
		{"the same option after trimming", []string{"Redis", " Redis "}},
		{"more options than anyone reads", many},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.AskClarification(ctx, p.ID, "coder", "which?", c.options, nil)
			var v *ValidationError
			if !errors.As(err, &v) {
				t.Fatalf("got %v, want a validation error the API renders as 400", err)
			}
		})
	}

	open, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d refused question(s) reached the operator anyway", len(open))
	}
}

// A timestamp somebody wrote by hand must not take the panel down with it.
//
// A real database had one: a question cancelled at a sqlite3 prompt, its
// answered_at left in SQLite's own format by `datetime('now')`. Reading is
// strict about the format it writes, and a listing fails whole, so that one
// row emptied every open question in the project rather than only itself.
func TestAHandPatchedTimestampDoesNotEmptyTheQueue(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AskClarification(ctx, p.ID, "coder", "which range?", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO clarifications (id, project_id, role, question, state, created_at, answered_at)
		 VALUES ('patched', ?, 'coder', 'which app?', 'open', datetime('now'), datetime('now'))`,
		p.ID); err != nil {
		t.Fatal(err)
	}

	open, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("%d question(s) reached the operator, want both", len(open))
	}

	patched, err := db.GetClarification(ctx, "patched")
	if err != nil {
		t.Fatalf("GetClarification: %v", err)
	}
	// datetime('now') is UTC and carries no zone, so that is how it reads back.
	if patched.CreatedAt.Location() != time.UTC || patched.CreatedAt.IsZero() {
		t.Errorf("created at %v, want a UTC time", patched.CreatedAt)
	}
	if patched.AnsweredAt == nil {
		t.Error("the hand-written answered_at read back as nothing")
	}
}

// A value that is not a timestamp at all still says so, rather than reading
// back as the zero time and dating a question to year one.
func TestAnUnreadableTimestampIsStillAnError(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO clarifications (id, project_id, role, question, state, created_at)
		 VALUES ('junk', ?, 'coder', 'which?', 'open', 'yesterday')`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetClarification(ctx, "junk"); err == nil {
		t.Error("a question dated \"yesterday\" was read without complaint")
	}
}

// Asking again is asking the same question.
//
// `zerg ask` gives up after its wait and reports the question as still open,
// and what the agent does with that is ask again. Watched on one card: two
// identical questions in the panel with nothing to tell them apart, two
// different options chosen six seconds apart, and the agent acting on the
// second having never seen the first.
func TestARepeatedQuestionIsTheSameQuestion(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Login", "build it", "coder")
	if err != nil {
		t.Fatal(err)
	}
	offered := []string{"Redis", "A signed cookie"}

	first, err := db.AskClarification(ctx, p.ID, "coder", "Where does the session live?", offered, &task.ID)
	if err != nil {
		t.Fatalf("AskClarification: %v", err)
	}
	again, err := db.AskClarification(ctx, p.ID, "coder", "Where does the session live?", offered, &task.ID)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("the repeat filed a second card (%s, then %s)", first.ID, again.ID)
	}

	open, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Errorf("the operator sees %d cards for one decision", len(open))
	}
}

// An answer typed a moment after the asker stopped listening is late, not
// lost: the next ask is handed it rather than filing a card asking again.
func TestAnAnswerThatArrivedLateReachesTheNextAsk(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	asked, err := db.AskClarification(ctx, p.ID, "coder", "Which app should I serve?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The operator answers after the agent's wait ran out, so nothing is
	// listening at the moment it lands.
	if err := db.AnswerClarification(ctx, asked.ID, "the admin one"); err != nil {
		t.Fatal(err)
	}

	again, err := db.AskClarification(ctx, p.ID, "coder", "Which app should I serve?", nil, nil)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if again.ID != asked.ID {
		t.Fatalf("the repeat filed a new card instead of collecting the answer")
	}
	if again.Answer == nil || *again.Answer != "the admin one" {
		t.Errorf("the repeat came back with %v, want the answer already given", again.Answer)
	}

	// Once it has been read, the question is finished: an agent coming back to
	// the same decision later is asking something new and deserves a new
	// answer rather than the one it already acted on.
	if err := db.MarkClarificationDelivered(ctx, asked.ID); err != nil {
		t.Fatal(err)
	}
	third, err := db.AskClarification(ctx, p.ID, "coder", "Which app should I serve?", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == asked.ID {
		t.Error("an answer that was already read was handed over a second time")
	}
	if third.State != ClarificationOpen {
		t.Errorf("the new question is %q, want open", third.State)
	}
}

// Only the same question counts as a repeat. A different card, a different
// wording or a different offer is a different decision, and collapsing them
// would answer one question with another's answer.
func TestADifferentQuestionIsNotARepeat(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(ctx, p.ID, "Login", "build it", "coder")
	if err != nil {
		t.Fatal(err)
	}
	base, err := db.AskClarification(ctx, p.ID, "coder", "Where does the session live?", []string{"Redis"}, &task.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name     string
		role     string
		question string
		options  []string
		taskID   *string
	}{
		{"another role asking it", "reviewer", "Where does the session live?", []string{"Redis"}, &task.ID},
		{"the same words about another card", "coder", "Where does the session live?", []string{"Redis"}, nil},
		{"a different question", "coder", "Where do the uploads live?", []string{"Redis"}, &task.ID},
		{"a different offer", "coder", "Where does the session live?", []string{"Redis", "A cookie"}, &task.ID},
		{"no offer at all", "coder", "Where does the session live?", nil, &task.ID},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.AskClarification(ctx, p.ID, c.role, c.question, c.options, c.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID == base.ID {
				t.Error("was collapsed into the first question, and would be answered by its answer")
			}
		})
	}
}
