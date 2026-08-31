package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

// The verb an agent actually types, over a real socket.
//
// --option is repeated rather than one comma-separated string: an option is a
// sentence a person reads off a screen and can contain a comma, and re-splitting
// one is how quoting bugs start. The order matters too, since it is the order
// the operator reads.
func TestAskCollectsRepeatedOptions(t *testing.T) {
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
	t.Cleanup(func() { _ = exec.Command("rm", "-f", socket).Run() })
	if err := srv.Listen(socket); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	t.Setenv("ZERG_SOCKET", socket)
	t.Setenv("ZERG_TOKEN", srv.Mint(p.ID, "coder"))

	// Nobody is going to answer, so do not wait for one.
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = out
	err = runAsk([]string{
		"--wait", "0",
		"--option", "Redis, shared across instances",
		"--option", "A signed cookie, no server state",
		"Where does the session live?",
	})
	os.Stdout = saved
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	open, err := db.ListOpenClarifications(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("%d question(s) reached the operator, want 1", len(open))
	}
	want := []string{"Redis, shared across instances", "A signed cookie, no server state"}
	if !slices.Equal(open[0].Options, want) {
		t.Errorf("options are %q, want %q, in the order they were typed", open[0].Options, want)
	}
	if open[0].Question != "Where does the session live?" {
		t.Errorf("question is %q", open[0].Question)
	}
}
