package store

import (
	"context"
	"testing"
	"time"
)

// A pinned task keeps its transcript, however old.
//
// The sweep is what makes history affordable, and it is also what would take
// the one card somebody wanted to read in six months. Pinning is the exemption,
// and it has to hold on the delete itself rather than in a caller.
func TestPruneKeepsAPinnedTasksTranscript(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	old := time.Now().Add(-90 * 24 * time.Hour)
	var kept, swept string
	for _, tc := range []struct {
		name   string
		pinned bool
	}{
		{"Worth keeping", true},
		{"Ordinary", false},
	} {
		task, err := db.CreateTask(ctx, p.ID, tc.name, "body", "")
		if err != nil {
			t.Fatal(err)
		}
		if tc.pinned {
			if err := db.SetTaskPinned(ctx, task.ID, true); err != nil {
				t.Fatalf("SetTaskPinned: %v", err)
			}
			kept = task.ID
		} else {
			swept = task.ID
		}
		if err := db.RecordEvent(ctx, &Event{
			ID: NewID(), ProjectID: p.ID, TaskID: &task.ID,
			Role: "coder", Kind: "message", At: old, Text: "did the thing",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// And an event belonging to no task at all, which is what a chat leaves.
	if err := db.RecordEvent(ctx, &Event{
		ID: NewID(), ProjectID: p.ID, Role: "chat", Kind: "message", At: old, Text: "hello",
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d events, want the unpinned card's and the chat's", n)
	}

	left, err := db.ListEvents(ctx, EventQuery{ProjectID: p.ID, Task: kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("a pinned card kept %d events, want its transcript", len(left))
	}
	gone, err := db.ListEvents(ctx, EventQuery{ProjectID: p.ID, Task: swept})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Errorf("an ordinary card kept %d events past the window", len(gone))
	}

	// Unpinning gives it back to the next sweep, or the pin would be a decision
	// with no way out.
	if err := db.SetTaskPinned(ctx, kept, false); err != nil {
		t.Fatal(err)
	}
	if n, err := db.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour)); err != nil || n != 1 {
		t.Errorf("after unpinning, swept %d events (%v), want the one it was holding", n, err)
	}
}
