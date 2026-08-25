package event

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/store"
)

// A burst must not cost rows. The recorder used to be an ordinary subscriber
// with a 1024 buffer, and the bus drops when a buffer fills — so a fast agent,
// or one slow SQLite commit, silently lost transcript lines and the usage rows
// the cost accounting is made of.
func TestABurstDoesNotLoseUsageRows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	p, err := db.CreateProject(ctx, t.TempDir(), "burst", "main")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	bus := NewBus()
	rec := Record(ctx, bus, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Comfortably more than the old 1024 buffer, published as fast as the bus
	// will take them. Not larger: the point is that nothing is dropped between
	// the bus and the queue, and every extra row past that only measures how
	// fast the runner commits to SQLite.
	const burst = 2000
	for range burst {
		bus.Publish(Event{
			Event: adapter.Event{
				Kind: adapter.EventUsage, TokensOut: 1, CostUSD: 0.001,
				Model: "m", Provider: "x", Text: "turn",
			},
			ProjectID: p.ID, Role: "coder", At: time.Now(),
		})
	}

	// Drained and written.
	deadline := time.Now().Add(120 * time.Second)
	var turns int
	for time.Now().Before(deadline) {
		rows, err := db.UsageByGroup(ctx, p.ID, "role", time.Time{})
		if err == nil {
			turns = 0
			for _, r := range rows {
				turns += r.Turns
			}
			if turns >= burst {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	st := rec.Stats()
	if st.Dropped != 0 {
		t.Errorf("the bus dropped %d events before the recorder saw them", st.Dropped)
	}
	if turns != burst {
		t.Errorf("stored %d usage turns of %d published (queued %d, failed %d)",
			turns, burst, st.Queued, st.Failed)
	}
}
