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
func TestPruneKeepsAPinnedTasksTranscriptAndEveryConversation(t *testing.T) {
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
	// And a conversation, which belongs to no task and is not a transcript of
	// work: there is no other copy of it, and it ends when the person ends it.
	for _, said := range []struct{ role, text string }{
		{OperatorRole, "why is the evaluator recursive?"},
		{ChatRole, "because the grammar is"},
	} {
		if err := db.RecordEvent(ctx, &Event{
			ID: NewID(), ProjectID: p.ID, Role: said.role, Kind: "message", At: old, Text: said.text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d events, want only the unpinned card's", n)
	}

	// The conversation is still there, both halves of it. It used to go with
	// the rest, so a chat emptied itself after a fortnight with nothing said.
	for _, role := range []string{OperatorRole, ChatRole} {
		said, err := db.ListEvents(ctx, EventQuery{ProjectID: p.ID, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		if len(said) != 1 {
			t.Errorf("%s kept %d of its messages, want 1", role, len(said))
		}
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
