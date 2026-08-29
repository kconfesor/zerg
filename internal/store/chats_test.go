package store

import (
	"context"
	"testing"
)

// Closing a tab takes everything that was only ever part of it, and nothing
// that was not.
//
// That is the whole contract of a tab: one conversation's transcript and its
// files go, and the conversation in the next tab is untouched. Getting this
// wrong in either direction is worse than not having tabs -- either a closed
// chat leaves its messages behind, or closing one empties another.
func TestClosingAConversationTakesItsOwnAndLeavesTheRest(t *testing.T) {
	ctx := context.Background()
	db, project := seeded(t)

	first, err := db.CreateChat(ctx, project.ID, "the parser")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateChat(ctx, project.ID, "deploying")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []*Chat{first, second} {
		for _, role := range []string{OperatorRole, ChatRole} {
			if err := db.RecordEvent(ctx, &Event{
				ID: NewID(), ProjectID: project.ID, Role: role, Kind: "message",
				Text: c.Title, ChatID: c.ID,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.AddArtifact(ctx, &Artifact{
			ProjectID: project.ID, Role: OperatorRole, Kind: ArtifactImage,
			Label: c.Title, Name: c.Title + ".png", SHA256: "digest-" + c.ID,
			MIME: "image/png", Bytes: 10, ChatID: c.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	orphans, err := db.DeleteChat(ctx, project.ID, first.ID)
	if err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	// Its file's bytes are nobody's now, and are handed back to be removed.
	if len(orphans) != 1 || orphans[0] != "digest-"+first.ID {
		t.Errorf("orphaned digests = %v, want the closed conversation's", orphans)
	}

	gone, err := db.ListEvents(ctx, EventQuery{ProjectID: project.ID, Chat: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Errorf("the closed conversation kept %d messages", len(gone))
	}

	kept, err := db.ListEvents(ctx, EventQuery{ProjectID: project.ID, Chat: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 {
		t.Errorf("the other conversation has %d of its 2 messages", len(kept))
	}

	left, err := db.ListChats(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != second.ID {
		t.Errorf("tabs left = %v, want only the one that was not closed", left)
	}

	// And the other tab's file is still there.
	art, err := db.ArtifactsForChat(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 1 {
		t.Errorf("the other conversation has %d of its files", len(art))
	}
}

// A conversation is addressed with its project, so naming somebody else's id
// reads nothing.
func TestAConversationCannotBeReachedFromAnotherProject(t *testing.T) {
	ctx := context.Background()
	db, mine := seeded(t)
	theirs, err := db.CreateProject(ctx, repoDir(t, "other"), "Other", "main")
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.CreateChat(ctx, mine.ID, "mine")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetChat(ctx, theirs.ID, c.ID); err == nil {
		t.Error("another project read this conversation by naming its id")
	}
	if _, err := db.DeleteChat(ctx, theirs.ID, c.ID); err == nil {
		t.Error("another project deleted this conversation by naming its id")
	}
	if _, err := db.GetChat(ctx, mine.ID, c.ID); err != nil {
		t.Errorf("the owning project cannot read it: %v", err)
	}
}

// The tab takes its name from the first thing said in it, and keeps a name a
// person typed.
func TestAConversationIsNamedByItsFirstMessageUnlessItHasAName(t *testing.T) {
	ctx := context.Background()
	db, project := seeded(t)

	unnamed, err := db.CreateChat(ctx, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.NameChat(ctx, unnamed.ID, "why is the evaluator recursive?"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetChat(ctx, project.ID, unnamed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "why is the evaluator recursive?" {
		t.Errorf("title = %q, want the first message", got.Title)
	}

	// A person's own name for it outranks whatever they opened with.
	if err := db.RenameChat(ctx, project.ID, unnamed.ID, "parser questions"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetChat(ctx, project.ID, unnamed.ID)
	if got.Title != "parser questions" {
		t.Errorf("title = %q, want the one that was typed", got.Title)
	}
}
