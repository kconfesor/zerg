package store

import (
	"context"
	"testing"
)

// A review is a conversation anchored to the code, and its state is what the
// gate reads.
func TestAReviewThreadHoldsTheConversationAndItsState(t *testing.T) {
	ctx := context.Background()
	db, p := seeded(t)
	task, err := db.CreateTask(ctx, p.ID, "Postfix operator", "add it", "")
	if err != nil {
		t.Fatal(err)
	}

	thread, err := db.OpenReviewThread(ctx, &ReviewThread{
		ProjectID: p.ID, TaskID: task.ID, CommitSHA: "abc1234", File: "src/parse.rs", Line: 41,
	}, OperatorRole, "why is this recursive?")
	if err != nil {
		t.Fatalf("OpenReviewThread: %v", err)
	}
	if thread.State != ThreadOpen || len(thread.Comments) != 1 {
		t.Fatalf("a new thread came back %+v", thread)
	}
	// The remark and the thread are written together: a thread with no comment
	// renders as an empty box and blocks a merge, which is the worst of both.
	if thread.Comments[0].Body != "why is this recursive?" {
		t.Errorf("the remark that opened it is %q", thread.Comments[0].Body)
	}

	// The gate's question.
	if n, err := db.OpenReviewThreads(ctx, task.ID); err != nil || n != 1 {
		t.Errorf("open threads = %d (%v), want 1", n, err)
	}

	// The role answers on the thread that asked, rather than in a fresh handoff
	// with no relation to the question.
	if _, err := db.AddReviewComment(ctx, thread.ID, "coder", "it mirrors parse_factor; flattened now"); err != nil {
		t.Fatalf("AddReviewComment: %v", err)
	}
	back, err := db.ReviewThread(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Comments) != 2 || back.Comments[1].Author != "coder" {
		t.Errorf("the answer did not land on the thread that asked: %+v", back.Comments)
	}
	// Answering is not settling. Only the reviewer closes a thread; an agent
	// resolving its own would be marking its own homework.
	if back.State != ThreadOpen {
		t.Error("answering a thread resolved it")
	}

	if err := db.SetReviewThreadState(ctx, thread.ID, true); err != nil {
		t.Fatalf("resolving: %v", err)
	}
	settled, err := db.ReviewThread(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != ThreadResolved || settled.ResolvedAt == nil {
		t.Errorf("a resolved thread came back %+v", settled)
	}
	if n, _ := db.OpenReviewThreads(ctx, task.ID); n != 0 {
		t.Errorf("%d threads still open after resolving the only one", n)
	}

	// A settled point can be reopened by commenting and unresolving: the
	// alternative is a new thread that has lost the context of the old one.
	if _, err := db.AddReviewComment(ctx, thread.ID, OperatorRole, "this came back"); err != nil {
		t.Errorf("commenting on a resolved thread: %v", err)
	}
	if err := db.SetReviewThreadState(ctx, thread.ID, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.OpenReviewThreads(ctx, task.ID); n != 1 {
		t.Error("reopening a thread did not put it back in front of the gate")
	}

	// Threads point at a file and a line, so a review can be read where the
	// code is rather than as a list of remarks about a whole diff.
	all, err := db.ReviewThreads(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].File != "src/parse.rs" || all[0].Line != 41 {
		t.Errorf("the thread lost its anchor: %+v", all)
	}

	// An empty remark is not a remark.
	if _, err := db.OpenReviewThread(ctx, &ReviewThread{ProjectID: p.ID, TaskID: task.ID}, OperatorRole, "  "); !isInvalid(err) {
		t.Errorf("an empty comment was accepted: %v", err)
	}

	// Deleting the card takes its review with it, the way its messages and its
	// transcript already go.
	if err := db.DeleteTask(ctx, p.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if left, err := db.ReviewThreads(ctx, task.ID); err != nil || len(left) != 0 {
		t.Errorf("a deleted card left %d threads behind (%v)", len(left), err)
	}
}
