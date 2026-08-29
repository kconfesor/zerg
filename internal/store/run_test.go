package store

import (
	"context"
	"testing"
)

// The runner's memory: prose, replaced rather than accumulated, and attributed
// so the next runner is told whether it is reading its own note or a
// correction.
func TestARunNoteIsWhatIsCurrentlyKnown(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)

	if _, err := db.RunNoteFor(ctx, p.ID); err == nil {
		t.Fatal("a note existed before anything learned anything")
	}

	if err := db.SaveRunNote(ctx, p.ID, "serves with: pnpm dev --port $PORT", "runner"); err != nil {
		t.Fatal(err)
	}
	note, err := db.RunNoteFor(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if note.Author != "runner" || note.Note != "serves with: pnpm dev --port $PORT" {
		t.Errorf("stored %+v", note)
	}

	// The operator correcting it wins, and is recorded as the author: "the
	// agent thinks this" and "you told it this" are different claims.
	if err := db.SaveRunNote(ctx, p.ID, "no: the admin portal, not the customer one",
		OperatorRole); err != nil {
		t.Fatal(err)
	}
	corrected, _ := db.RunNoteFor(ctx, p.ID)
	if corrected.Author != OperatorRole {
		t.Errorf("author = %q after a correction", corrected.Author)
	}
	if corrected.Note == note.Note {
		t.Error("the correction did not replace what was there")
	}

	if err := db.SaveRunNote(ctx, p.ID, "   ", "runner"); err == nil {
		t.Error("an empty note was accepted")
	}
}
