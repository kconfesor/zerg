package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	// A file rather than :memory:, because MaxOpenConns(1) plus an in-memory
	// DSN hides connection-scoping bugs that a real file would expose.
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := db.CreateTemplate(ctx, sampleTemplate("keeper")); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	db.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer db2.Close()

	if _, err := db2.GetTemplateByName(ctx, "keeper"); err != nil {
		t.Fatalf("reopening dropped data: %v", err)
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	in := sampleTemplate("coder")
	in.Args = []string{"--no-extensions", "--flag with space"}
	in.Prompt = "implement the thing"

	created, err := db.CreateTemplate(ctx, in)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateTemplate returned an empty id")
	}

	got, err := db.GetTemplate(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Name != "coder" || got.Prompt != "implement the thing" {
		t.Errorf("round trip changed the row: %+v", got)
	}
	// A flag containing a space is exactly why args are JSON, not a joined string.
	if len(got.Args) != 2 || got.Args[1] != "--flag with space" {
		t.Errorf("args round trip lost quoting: %q", got.Args)
	}
}

func TestGetMissingTemplateIsErrNotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.GetTemplate(context.Background(), "NOSUCHID")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDuplicateTemplateNameRejected(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if _, err := db.CreateTemplate(ctx, sampleTemplate("coder")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := db.CreateTemplate(ctx, sampleTemplate("coder")); err == nil {
		t.Fatal("a duplicate role name was accepted; the name is a worktree directory")
	}
}

func TestUpdateAndDeleteTemplate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	created, err := db.CreateTemplate(ctx, sampleTemplate("cleaner"))
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	created.Model = "opus"
	if err := db.UpdateTemplate(ctx, created); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}
	got, err := db.GetTemplate(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Model != "opus" {
		t.Errorf("model = %q, want opus", got.Model)
	}

	if err := db.DeleteTemplate(ctx, created.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if _, err := db.GetTemplate(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete, want ErrNotFound, got %v", err)
	}
	if err := db.DeleteTemplate(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice should report ErrNotFound, got %v", err)
	}
}

func TestValidateRejectsBadNames(t *testing.T) {
	// The name becomes a worktree directory, so these are path safety cases,
	// not stylistic ones.
	for _, name := range []string{"", "Coder", "co der", "../escape", "a/b", "-lead", "9lives",
		strings.Repeat("x", 32)} {
		tpl := sampleTemplate("placeholder")
		tpl.Name = name
		if err := tpl.Validate(); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	for _, name := range []string{"a", "coder", "code-reviewer", "qa2"} {
		tpl := sampleTemplate("placeholder")
		tpl.Name = name
		if err := tpl.Validate(); err != nil {
			t.Errorf("name %q was rejected: %v", name, err)
		}
	}
}

func TestValidateRejectsBadEnums(t *testing.T) {
	tpl := sampleTemplate("coder")
	tpl.Receive = "whenever"
	if err := tpl.Validate(); err == nil {
		t.Error("an unknown receive mode was accepted")
	}

	tpl = sampleTemplate("coder")
	tpl.Gate = "sometimes"
	if err := tpl.Validate(); err == nil {
		t.Error("an unknown gate was accepted")
	}

	tpl = sampleTemplate("coder")
	tpl.Receive = ReceiveBatch
	tpl.BatchMaxItems = 0
	if err := tpl.Validate(); err == nil {
		t.Error("an unbounded batch was accepted; unbounded batches starve priority work")
	}
}

func TestSeedIsIdempotentAndPreservesEdits(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	first, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(first) != len(builtinRoles) {
		t.Fatalf("seeded %d roles, want %d", len(first), len(builtinRoles))
	}

	// A user edits a built-in.
	planner, err := db.GetTemplateByName(ctx, "planner")
	if err != nil {
		t.Fatalf("planner missing: %v", err)
	}
	planner.Prompt = "my own spec instructions"
	if err := db.UpdateTemplate(ctx, planner); err != nil {
		t.Fatalf("UpdateTemplate: %v", err)
	}

	// Restarting must not clobber that edit — the whole point of config living
	// in the database rather than being copied from a file every launch.
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	again, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(again) != len(builtinRoles) {
		t.Errorf("second seed duplicated roles: %d", len(again))
	}
	planner, err = db.GetTemplateByName(ctx, "planner")
	if err != nil {
		t.Fatalf("planner missing after reseed: %v", err)
	}
	if planner.Prompt != "my own spec instructions" {
		t.Error("re-seeding overwrote a user's edit to a built-in role")
	}
}

// A role added to the library after someone installed zerg has to reach their
// database, or a new built-in only ever exists for people who start fresh.
//
// Seed inserts by name and skips what is already there, which covers both: an
// edited built-in is left alone, and one that is simply absent is added. This
// is the second half, and it is the half that is easy to lose by making Seed
// run only on a database it just created.
func TestSeedAddsRolesThatShippedLater(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// A database from before the debugger role existed.
	debugger, err := db.GetTemplateByName(ctx, "debugger")
	if err != nil {
		t.Fatalf("debugger missing from a fresh seed: %v", err)
	}
	if err := db.DeleteTemplate(ctx, debugger.ID); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}

	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("re-Seed: %v", err)
	}
	again, err := db.GetTemplateByName(ctx, "debugger")
	if err != nil {
		t.Fatalf("a role added to the library never reached an existing database: %v", err)
	}
	if again.Prompt == "" || !again.Builtin {
		t.Errorf("debugger came back as %+v, want a seeded built-in with its prompt", again)
	}
	tpls, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	if len(tpls) != len(builtinRoles) {
		t.Errorf("library holds %d roles, want %d", len(tpls), len(builtinRoles))
	}
}

func TestSeededLibraryShape(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	tpls, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}

	byName := map[string]RoleTemplate{}
	for _, tpl := range tpls {
		byName[tpl.Name] = tpl
		if !tpl.Builtin {
			t.Errorf("%s should be marked builtin", tpl.Name)
		}
		if strings.TrimSpace(tpl.Prompt) == "" {
			t.Errorf("%s shipped with an empty prompt", tpl.Name)
		}
		if err := tpl.Validate(); err != nil {
			t.Errorf("shipped role %s does not validate: %v", tpl.Name, err)
		}
	}

	// planner is the reason the approval gate exists as a field.
	if byName["planner"].Gate != GateApproval {
		t.Error("planner must gate on approval; it is the write-spec-then-approve flow")
	}
	// Reviewing roles run the stronger model on purpose.
	for _, n := range []string{"reviewer", "architect", "security"} {
		if byName[n].Model != "opus" {
			t.Errorf("%s runs %q; reviewing roles are meant to run opus", n, byName[n].Model)
		}
	}
	if byName["coder"].Receive != ReceiveTask {
		t.Error("coder should take one task at a time")
	}

	if _, err := db.GetSetting(ctx, SettingSharedInstructions); err != nil {
		t.Errorf("shared instructions were not seeded: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if _, err := db.GetSetting(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.SetSetting(ctx, "k", "one"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := db.SetSetting(ctx, "k", "two"); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	got, err := db.GetSetting(ctx, "k")
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "two" {
		t.Errorf("got %q, want two", got)
	}
}

func TestNewIDIsSortableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	ids := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		id := NewID()
		if len(id) != 26 {
			t.Fatalf("id %q is %d chars, want 26", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// Ids must sort by creation order even inside a single millisecond, which
	// is where a burst of events lands. Without monotonicity two events 0.2ms
	// apart would replay out of order.
	if !sort.StringsAreSorted(ids) {
		for i := 1; i < len(ids); i++ {
			if ids[i-1] >= ids[i] {
				t.Fatalf("ids went backwards at %d: %q then %q", i, ids[i-1], ids[i])
			}
		}
	}

	early := newIDAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := newIDAt(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC))
	if early >= late {
		t.Errorf("ids do not sort by time: %q >= %q", early, late)
	}
}

// A clock that steps backwards must not produce an id that sorts before one
// already handed out, or an event stream silently reorders itself.
func TestNewIDSurvivesAClockGoingBackwards(t *testing.T) {
	forward := newIDAt(time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	backward := newIDAt(time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC))
	if backward <= forward {
		t.Errorf("a backwards clock produced %q, which does not follow %q", backward, forward)
	}
}

func TestNewIDIsUniqueUnderConcurrency(t *testing.T) {
	const workers, each = 16, 500
	out := make(chan string, workers*each)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				out <- NewID()
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := map[string]bool{}
	for id := range out {
		if seen[id] {
			t.Fatalf("concurrent generation produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func sampleTemplate(name string) *RoleTemplate {
	return &RoleTemplate{
		Name:           name,
		Harness:        "claude",
		Model:          "sonnet",
		Args:           []string{},
		Receive:        ReceiveTask,
		BatchMaxItems:  8,
		BatchMaxAgeSec: 300,
		Gate:           GateNone,
	}
}

// Hiding must not disturb the work. The card is finished either way, and
// unhiding has to give back exactly what was put away — the temptation is to
// implement it as a lane or state change, which would lose that.
func TestHidingLeavesTheCardOtherwiseUntouched(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, t.TempDir(), "calc", "main")
	if err != nil {
		t.Fatal(err)
	}

	task, err := db.CreateTask(ctx, p.ID, "Doc", "write a doc", "coder")
	if err != nil {
		t.Fatal(err)
	}
	// A card still moving cannot be put away: that would hide the work rather
	// than the record of it.
	if err := db.SetTaskHidden(ctx, task.ID, true); err == nil {
		t.Fatal("hid a task that was not finished")
	}

	if _, err := db.sql.ExecContext(ctx,
		`UPDATE tasks SET state = 'done', lane = 'done' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SetTaskHidden(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	hidden, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hidden.Hidden {
		t.Error("hidden did not stick")
	}
	if hidden.State != before.State || hidden.Lane != before.Lane {
		t.Errorf("hiding moved the card: %s/%s became %s/%s",
			before.Lane, before.State, hidden.Lane, hidden.State)
	}

	if err := db.SetTaskHidden(ctx, task.ID, false); err != nil {
		t.Fatal(err)
	}
	back, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hidden || back.State != before.State || back.Lane != before.Lane {
		t.Errorf("unhiding did not restore the card: %+v", back)
	}
}

// A database somewhere this process cannot chmod still opens.
//
// The mode is a precaution, not a precondition: the file is already there with
// whatever mode it has, so refusing the open protects nothing and costs the
// operator their daemon. Pointing --db at a directory owned by someone else —
// /tmp is the obvious one — did exactly that.
func TestOpenSucceedsWhereModesCannotBeSet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A directory this process may write into but not chmod is hard to
	// arrange portably, so the check is on the behaviour that broke: opening
	// under a directory whose mode is left alone must not fail, and the
	// database must be usable afterwards.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Skipf("cannot set up the fixture: %v", err)
	}
	db, err := Open(ctx, filepath.Join(dir, "loose.db"))
	if err != nil {
		t.Fatalf("Open refused a database it could not fully secure: %v", err)
	}
	defer db.Close()

	if _, err := db.CreateProject(ctx, repoDir(t, "loose"), "loose", "main"); err != nil {
		t.Errorf("the database opened but does not work: %v", err)
	}
}
