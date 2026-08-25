package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// readFrames pulls SSE frames until it has n "activity" events or gives up.
func readFrames(t *testing.T, body *bufio.Reader, n int) []string {
	t.Helper()
	var ids []string
	for len(ids) < n {
		line, err := body.ReadString('\n')
		if err != nil {
			return ids
		}
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimSpace(strings.TrimPrefix(line, "id: ")))
		}
	}
	return ids
}

// The stream must replay history and then keep going, with nothing lost in the
// join between the two and nothing sent twice.
func TestEventStreamReplaysThenTails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := newFixture(t)
	bus := event.NewBus()
	f.deps.Bus = bus
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	// Two events already on the record.
	for i := 0; i < 2; i++ {
		if err := f.db.RecordEvent(ctx, &store.Event{
			ProjectID: f.project.ID, Role: "coder", Kind: "message", Text: "before",
		}); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}

	req, _ := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/projects/"+f.project.ID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body := bufio.NewReader(resp.Body)
	replayed := readFrames(t, body, 2)
	if len(replayed) != 2 {
		t.Fatalf("replayed %d events, want the 2 on the record", len(replayed))
	}

	// Now a live one. It must arrive on the same connection.
	live := store.NewID()
	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.Publish(event.Event{
			Event:     adapter.Event{Kind: adapter.EventMessage, Text: "after"},
			ID:        live,
			ProjectID: f.project.ID,
			Role:      "coder",
			At:        time.Now(),
		})
	}()

	got := readFrames(t, body, 1)
	if len(got) != 1 || got[0] != live {
		t.Fatalf("live event = %v, want [%s]; the tail did not follow the replay", got, live)
	}
	for _, id := range replayed {
		if id == live {
			t.Error("an event was sent twice across the replay/tail boundary")
		}
	}
}

// Last-Event-ID is what a browser resends after a dropped connection. Honouring
// it is the whole reason SSE needs no reconnect logic of its own.
func TestEventStreamResumesFromLastEventID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
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

	req, _ := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/projects/"+f.project.ID+"/events", nil)
	req.Header.Set("Last-Event-ID", ids[1]) // saw the first two
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	got := readFrames(t, bufio.NewReader(resp.Body), 2)
	if len(got) != 2 || got[0] != ids[2] || got[1] != ids[3] {
		t.Errorf("resumed with %v, want the two after the cursor %v", got, ids[2:])
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

// The replay query and the live subscription overlap on purpose: the
// subscription opens first, so an event landing mid-replay is buffered rather
// than lost. The cost is that such an event is in both the table and the
// channel, and the stream must send it exactly once.
//
// This drives that overlap directly, by putting one id on both paths.
func TestEventAppearingInBothReplayAndTailIsSentOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := newFixture(t)
	bus := event.NewBus()
	f.deps.Bus = bus
	srv := httptest.NewServer(New(f.deps).Routes())
	defer srv.Close()

	// One event on the record, which the replay will send.
	stored := &store.Event{ProjectID: f.project.ID, Role: "coder", Kind: "message"}
	if err := f.db.RecordEvent(ctx, stored); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/projects/"+f.project.ID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)

	if got := readFrames(t, body, 1); len(got) != 1 || got[0] != stored.ID {
		t.Fatalf("replay sent %v, want [%s]", got, stored.ID)
	}

	// The same event now arrives on the bus, exactly as it would had it been
	// published while the replay query was running. It must be suppressed.
	later := store.NewID()
	go func() {
		bus.Publish(event.Event{
			Event: adapter.Event{Kind: adapter.EventMessage},
			ID:    stored.ID, ProjectID: f.project.ID, Role: "coder", At: time.Now(),
		})
		time.Sleep(50 * time.Millisecond)
		// A genuinely new one, to prove the stream is still live and to give
		// the read below something to terminate on.
		bus.Publish(event.Event{
			Event: adapter.Event{Kind: adapter.EventMessage},
			ID:    later, ProjectID: f.project.ID, Role: "coder", At: time.Now(),
		})
	}()

	got := readFrames(t, body, 1)
	if len(got) != 1 || got[0] != later {
		t.Errorf("next frame was %v, want [%s]; the replayed event was sent twice", got, later)
	}
}
