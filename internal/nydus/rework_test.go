package nydus

import (
	"context"
	"testing"
)

// Can a reviewer hand work back to the role that produced it?
func TestReviewerCanReturnWorkToCoder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	// planner -> coder (approve the gated handoff)
	l, _ := f.n.Claim(ctx, f.project.ID, "planner")
	f.n.Send(ctx, f.project.ID, "planner", SendRequest{TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa"})
	f.n.Ack(ctx, l.ID)
	pend, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.n.Approve(ctx, pend[0].ID)

	// coder -> reviewer
	l, _ = f.n.Claim(ctx, f.project.ID, "coder")
	f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb"})
	f.n.Ack(ctx, l.ID)

	// reviewer claims and finds problems -> sends BACK to coder
	l, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || l == nil {
		t.Fatalf("reviewer Claim: %v", err)
	}
	if l.Items[0].FromRole != "coder" {
		t.Fatalf("envelope says from=%q; the reviewer needs this to know who to return to", l.Items[0].FromRole)
	}
	_, err = f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, To: "coder", Commit: "cccccccccc",
		Body: "error paths are untested; see src/eval.rs:42",
	})
	if err != nil {
		t.Fatalf("reviewer could NOT send work back to coder: %v", err)
	}
	f.n.Ack(ctx, l.ID)

	// Where did the card go?
	got := f.reload(t, task.ID)
	t.Logf("after bounce-back: lane=%s state=%s", got.Lane, got.State)
	if got.Lane != "coder" {
		t.Errorf("card is in %q, want coder", got.Lane)
	}

	// Does coder actually receive it, with the reviewer's note?
	l2, err := f.n.Claim(ctx, f.project.ID, "coder")
	if err != nil || l2 == nil {
		t.Fatalf("coder did not receive the returned work: %v", err)
	}
	if l2.Items[0].FromRole != "reviewer" {
		t.Errorf("returned work came from %q", l2.Items[0].FromRole)
	}
	t.Logf("coder received: from=%s body=%q", l2.Items[0].FromRole, l2.Items[0].Body)

	// One backward hop counted as rework; the forward hops did not.
	if got.ReworkCount != 1 {
		t.Errorf("reworkCount = %d after one bounce, want 1", got.ReworkCount)
	}

	// And it can go round again — rework is allowed, just counted.
	if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
		TaskID: task.ID, To: "reviewer", Commit: "dddddddddd",
	}); err != nil {
		t.Errorf("coder could not re-submit after fixing: %v", err)
	}
	if after := f.reload(t, task.ID); after.ReworkCount != 1 {
		t.Errorf("reworkCount = %d after re-submitting forward, want it unchanged at 1", after.ReworkCount)
	}
}

// Forward handoffs are the normal path and must never look like a loop.
func TestForwardHandoffsAreNotRework(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	l, _ := f.n.Claim(ctx, f.project.ID, "planner")
	f.n.Send(ctx, f.project.ID, "planner", SendRequest{TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa"})
	f.n.Ack(ctx, l.ID)
	pend, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.n.Approve(ctx, pend[0].ID)

	l, _ = f.n.Claim(ctx, f.project.ID, "coder")
	f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb"})
	f.n.Ack(ctx, l.ID)

	if got := f.reload(t, task.ID); got.ReworkCount != 0 {
		t.Errorf("reworkCount = %d after only forward hops, want 0", got.ReworkCount)
	}
}

// Laps accumulate, and a card over the threshold is raised for a human.
func TestRepeatedLoopsCrossTheThreshold(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	// Get it into coder's hands once.
	l, _ := f.n.Claim(ctx, f.project.ID, "planner")
	f.n.Send(ctx, f.project.ID, "planner", SendRequest{TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa"})
	f.n.Ack(ctx, l.ID)
	pend, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.n.Approve(ctx, pend[0].ID)

	// Three laps of coder <-> reviewer.
	for i := 0; i < 3; i++ {
		lc, err := f.n.Claim(ctx, f.project.ID, "coder")
		if err != nil || lc == nil {
			t.Fatalf("lap %d: coder Claim: %v", i, err)
		}
		if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
			TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb",
		}); err != nil {
			t.Fatalf("lap %d: coder Send: %v", i, err)
		}
		f.n.Ack(ctx, lc.ID)

		lr, err := f.n.Claim(ctx, f.project.ID, "reviewer")
		if err != nil || lr == nil {
			t.Fatalf("lap %d: reviewer Claim: %v", i, err)
		}
		if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
			TaskID: task.ID, To: "coder", Commit: "cccccccccc", Body: "still not right",
		}); err != nil {
			t.Fatalf("lap %d: reviewer Send: %v", i, err)
		}
		f.n.Ack(ctx, lr.ID)
	}

	got := f.reload(t, task.ID)
	if got.ReworkCount != 3 {
		t.Fatalf("reworkCount = %d after three laps, want 3", got.ReworkCount)
	}

	threshold := f.db.ReworkThreshold(ctx)
	looping, err := f.db.ListReworkedTasks(ctx, f.project.ID, threshold)
	if err != nil {
		t.Fatalf("ListReworkedTasks: %v", err)
	}
	if len(looping) != 1 || looping[0].ID != task.ID {
		t.Fatalf("a card at %d laps was not raised for a human: %+v", got.ReworkCount, looping)
	}

	// Below the threshold nothing is raised: rework is normal, and crying
	// about the first bounce would train everyone to ignore the panel.
	if quiet, _ := f.db.ListReworkedTasks(ctx, f.project.ID, 99); len(quiet) != 0 {
		t.Error("a card was raised below the threshold")
	}
}

// A card that took several laps and then shipped is history, not a decision
// anyone still needs to make.
func TestFinishedCardsAreNotRaised(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	l, _ := f.n.Claim(ctx, f.project.ID, "planner")
	f.n.Send(ctx, f.project.ID, "planner", SendRequest{TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa"})
	f.n.Ack(ctx, l.ID)
	pend, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.n.Approve(ctx, pend[0].ID)

	lc, _ := f.n.Claim(ctx, f.project.ID, "coder")
	f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb"})
	f.n.Ack(ctx, lc.ID)
	lr, _ := f.n.Claim(ctx, f.project.ID, "reviewer")
	f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: task.ID, To: "coder", Commit: "cccccccccc"})
	f.n.Ack(ctx, lr.ID)

	// coder fixes it, reviewer accepts and finishes.
	lc, _ = f.n.Claim(ctx, f.project.ID, "coder")
	f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, To: "reviewer", Commit: "dddddddddd"})
	f.n.Ack(ctx, lc.ID)
	lr, _ = f.n.Claim(ctx, f.project.ID, "reviewer")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "eeeeeeeeee",
	}); err != nil {
		t.Fatalf("completion: %v", err)
	}
	f.n.Ack(ctx, lr.ID)

	looping, err := f.db.ListReworkedTasks(ctx, f.project.ID, 1)
	if err != nil {
		t.Fatalf("ListReworkedTasks: %v", err)
	}
	if len(looping) != 0 {
		t.Errorf("a finished card is still being raised: %+v", looping)
	}
}
