package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

// The verb an agent actually types, in the order every example writes it.
//
// Go's flag package stops at the first word that is not a flag, and the
// documented form puts the question there. Parsed straight through, `--task`
// and every `--option` after it were positional and silently dropped: a
// question filed against no card offering none of the answers it had just
// worked out, which is exactly the retyping this was meant to end.
func TestAskTakesFlagsAfterTheQuestion(t *testing.T) {
	ctx := context.Background()
	f := newAskFixture(t)

	task, err := f.db.CreateTask(ctx, f.project.ID, "Login", "build it", "coder")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Verbatim the shape seed.go, the usage text and ARCHITECTURE all print.
	askCLI(t, []string{
		"Where does the session live?",
		"--task", "Login",
		"--option", "Redis, shared across instances",
		"--option", "A signed cookie, no server state",
		"--wait", "0",
	})

	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("%d question(s) reached the operator, want 1", len(open))
	}
	got := open[0]
	if got.Question != "Where does the session live?" {
		t.Errorf("question is %q", got.Question)
	}
	want := []string{"Redis, shared across instances", "A signed cookie, no server state"}
	if !slices.Equal(got.Options, want) {
		t.Errorf("options are %q, want %q, in the order they were typed", got.Options, want)
	}
	if got.TaskID == nil || *got.TaskID != task.ID {
		t.Errorf("question is filed against %v, want the card it names", got.TaskID)
	}
}

// Flags first still works, since that is what anything already written does.
func TestAskTakesFlagsBeforeTheQuestion(t *testing.T) {
	ctx := context.Background()
	f := newAskFixture(t)

	askCLI(t, []string{
		"--wait", "0",
		"--option", "Redis, shared across instances",
		"--option", "A signed cookie, no server state",
		"Where does the session live?",
	})

	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 || len(open[0].Options) != 2 {
		t.Fatalf("got %+v, want one question with both options", open)
	}
}

// A question nobody quoted is the shell slip an agent actually makes. Keeping
// the first word alone files a question nobody can answer.
func TestAnUnquotedQuestionIsKeptWhole(t *testing.T) {
	ctx := context.Background()
	f := newAskFixture(t)

	askCLI(t, []string{"should", "this", "be", "idempotent?", "--wait", "0"})

	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 || open[0].Question != "should this be idempotent?" {
		t.Fatalf("got %+v, want the whole question", open)
	}
}

// The same parse, and the same documented order: ARCHITECTURE 13.2 prints
// `zerg artifact add ./report.html --label "Coverage report"`, and the label
// was going nowhere.
func TestArtifactAddTakesFlagsAfterThePath(t *testing.T) {
	ctx := context.Background()
	f := newAskFixture(t)

	task, err := f.db.CreateTask(ctx, f.project.ID, "Login", "build it", "coder")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// Inside the project: an artifact is something the work produced, and the
	// daemon refuses a path from anywhere else.
	report := filepath.Join(f.project.Path, "report.html")
	if err := os.WriteFile(report, []byte("<h1>coverage</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}

	quiet(t, func() error {
		return runArtifact([]string{"add", report, "--label", "Coverage report", "--task", "Login"})
	})

	made, err := f.db.ArtifactsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ArtifactsForTask: %v", err)
	}
	if len(made) != 1 {
		t.Fatalf("%d artifact(s) recorded, want 1", len(made))
	}
	if made[0].Label != "Coverage report" {
		t.Errorf("label is %q, want the one the command was given", made[0].Label)
	}
}

type askFixture struct {
	db      *store.DB
	project *store.Project
}

func newAskFixture(t *testing.T) *askFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p, err := db.CreateProject(ctx, t.TempDir(), "demo", "main")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	srv := agent.NewServer(db, nydus.New(db), artifact.New(t.TempDir()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// macOS caps a unix socket path near 104 bytes, which t.TempDir() alone
	// can exceed.
	socket := filepath.Join("/tmp", store.NewID()[:12]+".sock")
	t.Cleanup(func() { _ = os.Remove(socket) })
	if err := srv.Listen(socket); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	t.Setenv("ZERG_SOCKET", socket)
	t.Setenv("ZERG_TOKEN", srv.Mint(p.ID, "coder"))
	return &askFixture{db: db, project: p}
}

// askCLI runs the verb the way an agent runs it, stdout out of the way.
func askCLI(t *testing.T, args []string) {
	t.Helper()
	quiet(t, func() error { return runAsk(args) })
}

func quiet(t *testing.T, run func() error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = saved }()
	if err := run(); err != nil {
		t.Fatalf("running the command: %v", err)
	}
}
