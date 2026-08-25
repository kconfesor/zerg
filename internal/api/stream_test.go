package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/adapter/claudeharness"
	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/store"
)

// streamFixture is a server with a bus attached and one project to stream.
type streamFixture struct {
	db      *store.DB
	project *store.Project
	deps    Deps
}

func newFixture(t *testing.T) *streamFixture {
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
	project, err := db.CreateProject(ctx, filepath.Join(t.TempDir(), "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	reg := adapter.NewRegistry()
	reg.Register(claudeharness.New())
	return &streamFixture{
		db: db, project: project,
		deps: Deps{DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: reg},
	}
}

// Opening the view should show what just happened, not the beginning of
// recorded history — but still in reading order.
func TestListEventsWithoutCursorReturnsTheNewestInOrder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	var ids []string
	for i := 0; i < 5; i++ {
		e := &store.Event{ProjectID: f.project.ID, Role: "coder", Kind: "message"}
		if err := f.db.RecordEvent(ctx, e); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
		ids = append(ids, e.ID)
	}

	got, err := f.db.ListEvents(ctx, store.EventQuery{ProjectID: f.project.ID, Limit: 2})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 2 || got[0].ID != ids[3] || got[1].ID != ids[4] {
		t.Errorf("got %d events starting %v; want the last two, oldest first", len(got), got)
	}
}

// Retention drops transcripts and must leave everything else standing.
func TestPruneKeepsRecentAndReportsWhatWent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	old := &store.Event{
		ProjectID: f.project.ID, Role: "coder", Kind: "message",
		At: time.Now().Add(-72 * time.Hour),
	}
	if err := f.db.RecordEvent(ctx, old); err != nil {
		t.Fatal(err)
	}
	recent := &store.Event{ProjectID: f.project.ID, Role: "coder", Kind: "message"}
	if err := f.db.RecordEvent(ctx, recent); err != nil {
		t.Fatal(err)
	}

	n, err := f.db.PruneEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1; a silent count is indistinguishable from complete history", n)
	}

	left, err := f.db.ListEvents(ctx, store.EventQuery{ProjectID: f.project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != recent.ID {
		t.Errorf("retention removed the wrong rows: %v", left)
	}
}

// ── the stream ────────────────────────────────────────────────────────────

// dial opens a stream and subscribes, returning the connection.
func dial(t *testing.T, ctx context.Context, srv *httptest.Server,
	projectID string, f clientFrame) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/projects/" + projectID + "/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	f.Type = "subscribe"
	if err := wsjson.Write(ctx, conn, f); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return conn
}

// readUntilCaughtUp collects activity ids up to the caught-up marker.
func readUntilCaughtUp(t *testing.T, ctx context.Context, conn *websocket.Conn) []string {
	t.Helper()
	var ids []string
	for {
		var f serverFrame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			t.Fatalf("read: %v", err)
		}
		switch f.Type {
		case "activity":
			ids = append(ids, f.Event.ID)
		case "caught-up":
			return ids
		case "error":
			t.Fatalf("server reported: %s", f.Message)
		}
	}
}

func readActivity(t *testing.T, ctx context.Context, conn *websocket.Conn) string {
	t.Helper()
	for {
		var f serverFrame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			t.Fatalf("read: %v", err)
		}
		if f.Type == "activity" {
			return f.Event.ID
		}
	}
}

// The stream replays history and then keeps going, with nothing lost in the
// join between the two and nothing sent twice.
func TestStreamReplaysThenTails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newFixture(t)
	bus := event.NewBus()
	f.deps.Bus = bus
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	for i := 0; i < 2; i++ {
		if err := f.db.RecordEvent(ctx, &store.Event{
			ProjectID: f.project.ID, Role: "coder", Kind: "message", Text: "before",
		}); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	conn := dial(t, ctx, srv, f.project.ID, clientFrame{})
	replayed := readUntilCaughtUp(t, ctx, conn)
	if len(replayed) != 2 {
		t.Fatalf("replayed %d events, want the 2 on the record", len(replayed))
	}

	live := store.NewID()
	bus.Publish(event.Event{
		Event: adapter.Event{Kind: adapter.EventMessage, Text: "after"},
		ID:    live, ProjectID: f.project.ID, Role: "coder", At: time.Now(),
	})

	if got := readActivity(t, ctx, conn); got != live {
		t.Errorf("live event = %s, want %s; the tail did not follow the replay", got, live)
	}
}

// A reconnect resends the cursor, and the server sends only what came after.
// EventSource did this via Last-Event-ID; on a socket it is explicit, and it is
// the whole reason a dropped connection is invisible rather than a hole.
func TestStreamResumesFromTheCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newFixture(t)
	f.deps.Bus = event.NewBus()
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	var ids []string
	for i := 0; i < 4; i++ {
		e := &store.Event{ProjectID: f.project.ID, Role: "coder", Kind: "message"}
		if err := f.db.RecordEvent(ctx, e); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
		ids = append(ids, e.ID)
	}

	conn := dial(t, ctx, srv, f.project.ID, clientFrame{After: ids[1]})
	got := readUntilCaughtUp(t, ctx, conn)
	if len(got) != 2 || got[0] != ids[2] || got[1] != ids[3] {
		t.Errorf("resumed with %v, want the two after the cursor", got)
	}
}

// The subscription opens before the replay query on purpose, so an event
// landing mid-replay is buffered rather than lost. The cost is that such an
// event is on both paths, and it must be written exactly once.
func TestStreamSendsAnOverlappingEventOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newFixture(t)
	bus := event.NewBus()
	f.deps.Bus = bus
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	stored := &store.Event{ProjectID: f.project.ID, Role: "coder", Kind: "message"}
	if err := f.db.RecordEvent(ctx, stored); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	conn := dial(t, ctx, srv, f.project.ID, clientFrame{})
	if got := readUntilCaughtUp(t, ctx, conn); len(got) != 1 || got[0] != stored.ID {
		t.Fatalf("replay sent %v, want [%s]", got, stored.ID)
	}

	// The same event now arrives on the bus, as it would had it been published
	// while the replay query ran. It must be suppressed.
	bus.Publish(event.Event{
		Event: adapter.Event{Kind: adapter.EventMessage},
		ID:    stored.ID, ProjectID: f.project.ID, Role: "coder", At: time.Now(),
	})
	later := store.NewID()
	bus.Publish(event.Event{
		Event: adapter.Event{Kind: adapter.EventMessage},
		ID:    later, ProjectID: f.project.ID, Role: "coder", At: time.Now(),
	})

	if got := readActivity(t, ctx, conn); got != later {
		t.Errorf("next frame was %s, want %s; the replayed event was sent twice", got, later)
	}
}

// A role filter must hold for live events too, not only the replay.
func TestStreamHonoursTheRoleFilterWhileLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newFixture(t)
	bus := event.NewBus()
	f.deps.Bus = bus
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	conn := dial(t, ctx, srv, f.project.ID, clientFrame{Role: "reviewer"})
	readUntilCaughtUp(t, ctx, conn)

	bus.Publish(event.Event{
		Event: adapter.Event{Kind: adapter.EventMessage}, ID: store.NewID(),
		ProjectID: f.project.ID, Role: "coder", At: time.Now(),
	})
	wanted := store.NewID()
	bus.Publish(event.Event{
		Event: adapter.Event{Kind: adapter.EventMessage}, ID: wanted,
		ProjectID: f.project.ID, Role: "reviewer", At: time.Now(),
	})

	if got := readActivity(t, ctx, conn); got != wanted {
		t.Errorf("got %s, want %s; another role's event crossed the filter", got, wanted)
	}
}
