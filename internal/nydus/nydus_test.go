package nydus

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/konfessor/zerg/internal/store"
)

// fakeIntegrator records merges so a test can assert integration happened
// without needing a repository.
type fakeIntegrator struct {
	mu     sync.Mutex
	merges []string
	err    error
}

func (f *fakeIntegrator) Merge(_ context.Context, _, _, commit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.merges = append(f.merges, commit)
	return nil
}

type fixture struct {
	db      *store.DB
	n       *Nydus
	project *store.Project
	clock   *fakeClock
	git     *fakeIntegrator
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond) // monotonic, so ids and ordering stay stable
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newFixture builds a project whose team is planner → coder → reviewer, all
// enabled, with reviewer terminal.
func newFixture(t *testing.T, opts ...Option) *fixture {
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
	p, err := db.CreateProject(ctx, filepath.Join(t.TempDir(), "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	id := func(name string) string {
		tpl, err := db.GetTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("GetTemplateByName(%q): %v", name, err)
		}
		return tpl.ID
	}
	if err := db.SetTeam(ctx, p.ID, []store.ProjectRole{
		{TemplateID: id("planner"), Enabled: true},
		{TemplateID: id("coder"), Enabled: true},
		{TemplateID: id("reviewer"), Enabled: true},
	}); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	clock := &fakeClock{t: time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)}
	git := &fakeIntegrator{}
	all := append([]Option{WithClock(clock.now), WithIntegrator(git)}, opts...)

	return &fixture{db: db, n: New(db, all...), project: p, clock: clock, git: git}
}

func (f *fixture) task(t *testing.T, name string) *store.Task {
	t.Helper()
	task, err := f.n.NewTask(context.Background(), f.project.ID, name, "do the thing")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	return task
}

func (f *fixture) reload(t *testing.T, id string) *store.Task {
	t.Helper()
	task, err := f.db.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

// ── the happy path ────────────────────────────────────────────────────────

func TestPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// The operator opens a card. It is queued for the first role, and nobody
	// has looked at it yet.
	task := f.task(t, "Calculator")
	if task.Lane != "planner" || task.State != store.TaskQueued {
		t.Fatalf("new card is %s/%s, want planner/queued", task.Lane, task.State)
	}

	// planner claims. The card is now genuinely being worked.
	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil {
		t.Fatalf("planner Claim: %v", err)
	}
	if lease == nil {
		t.Fatal("planner found no work, but a task was queued for it")
	}
	if got := f.reload(t, task.ID); got.State != store.TaskWorking {
		t.Errorf("state = %s, want working once claimed", got.State)
	}

	// planner hands to coder. planner gates on approval, so this is held.
	if _, err := f.n.Send(ctx, f.project.ID, "planner", SendRequest{
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa",
	}); err != nil {
		t.Fatalf("planner Send: %v", err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("planner Ack: %v", err)
	}

	// coder must not see it yet: a gated handoff waits for a human.
	if l, err := f.n.Claim(ctx, f.project.ID, "coder"); err != nil {
		t.Fatalf("coder Claim: %v", err)
	} else if l != nil {
		t.Fatal("coder claimed a handoff that was still awaiting approval")
	}
	if got := f.reload(t, task.ID); got.Lane != "planner" {
		t.Errorf("lane = %s; a held handoff must not move the card", got.Lane)
	}

	// Approve it.
	pending, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d approvals pending, want 1", len(pending))
	}
	if pending[0].TaskName != "Calculator" {
		t.Errorf("approval names task %q; Attention must show the card, not an id", pending[0].TaskName)
	}
	if err := f.n.Approve(ctx, pending[0].ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := f.reload(t, task.ID); got.Lane != "coder" || got.State != store.TaskQueued {
		t.Errorf("after approval card is %s/%s, want coder/queued", got.Lane, got.State)
	}

	// coder works and hands to reviewer.
	lease, err = f.n.Claim(ctx, f.project.ID, "coder")
	if err != nil || lease == nil {
		t.Fatalf("coder Claim: %v (lease %v)", err, lease)
	}
	if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
		TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb",
	}); err != nil {
		t.Fatalf("coder Send: %v", err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("coder Ack: %v", err)
	}

	// reviewer is terminal: it finishes rather than forwarding.
	lease, err = f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || lease == nil {
		t.Fatalf("reviewer Claim: %v (lease %v)", err, lease)
	}
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "cccccccccc",
	}); err != nil {
		t.Fatalf("reviewer completion: %v", err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("reviewer Ack: %v", err)
	}

	done := f.reload(t, task.ID)
	if done.Lane != store.LaneDone || done.State != store.TaskDone {
		t.Errorf("finished card is %s/%s, want done/done", done.Lane, done.State)
	}
	if done.CompletedAt == nil {
		t.Error("a finished card must record when it finished")
	}
	if done.ActiveMS <= 0 {
		t.Error("worked time did not accrue across the pipeline")
	}
	// Integration belongs to the orchestrator, not to the last agent.
	if len(f.git.merges) != 1 || f.git.merges[0] != "cccccccccc" {
		t.Errorf("merges = %v, want exactly the terminal commit", f.git.merges)
	}
}

// ── claiming ──────────────────────────────────────────────────────────────

// Two claimers must never receive the same work. The predecessor listed a
// directory and then moved a file, so the loser threw a stack trace — or in
// batch mode split the queue into two directories with no recovery path.
func TestConcurrentClaimsNeverDoubleTake(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")

	const claimers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		leases  []*store.Lease
		errored []error
	)
	start := make(chan struct{})
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := f.n.Claim(ctx, f.project.ID, "planner")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errored = append(errored, err)
			} else if l != nil {
				leases = append(leases, l)
			}
		}()
	}
	close(start)
	wg.Wait()

	// One unit of work exists. Whether a caller gets it or gets nil, no two
	// callers may hold different leases over it.
	ids := map[string]bool{}
	for _, l := range leases {
		ids[l.ID] = true
	}
	if len(ids) > 1 {
		t.Fatalf("%d distinct leases were granted over one message: %v", len(ids), ids)
	}
	for _, err := range errored {
		if !errors.Is(err, errRaced) {
			t.Errorf("unexpected claim error: %v", err)
		}
	}
}

// A role holding unacknowledged work resumes it rather than being handed a
// second unit — otherwise the first would be silently abandoned.
func TestClaimResumesAnOpenLease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")

	first, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || first == nil {
		t.Fatalf("first Claim: %v", err)
	}
	second, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("second claim returned %v, want the same open lease %s", second, first.ID)
	}
	if len(second.Items) != len(first.Items) {
		t.Errorf("resumed lease carries %d items, want %d", len(second.Items), len(first.Items))
	}
}

func TestClaimOnEmptyQueueIsNilNotError(t *testing.T) {
	f := newFixture(t)
	lease, err := f.n.Claim(context.Background(), f.project.ID, "coder")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease != nil {
		t.Fatal("an empty queue produced a lease")
	}
}

// ── leases ────────────────────────────────────────────────────────────────

// This is the predecessor's permanent stall, made recoverable: an agent that
// dies holding work must not take the work with it.
func TestExpiredLeaseReturnsWorkToTheQueue(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, WithLease(time.Minute))
	task := f.task(t, "Calculator")

	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}

	// Nothing has lapsed yet.
	if n, err := f.n.ExpireLeases(ctx); err != nil || n != 0 {
		t.Fatalf("premature expiry: n=%d err=%v", n, err)
	}

	f.clock.advance(2 * time.Minute)
	n, err := f.n.ExpireLeases(ctx)
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d leases, want 1", n)
	}
	if got := f.reload(t, task.ID); got.State != store.TaskQueued {
		t.Errorf("task state = %s, want queued once its lease lapsed", got.State)
	}

	// The work is claimable again, and identical to what was lost.
	again, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || again == nil {
		t.Fatalf("re-claim after expiry: %v", err)
	}
	if again.ID == lease.ID {
		t.Error("expiry handed back the same lease instead of granting a new one")
	}
	if len(again.Items) != 1 || again.Items[0].ID != lease.Items[0].ID {
		t.Error("requeued work is not the message that was lost")
	}
}

func TestAckOnExpiredLeaseIsRejected(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, WithLease(time.Minute))
	f.task(t, "Calculator")

	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	f.clock.advance(2 * time.Minute)
	if _, err := f.n.ExpireLeases(ctx); err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}

	// The work belongs to someone else now, so acknowledging it must fail
	// loudly rather than close a lease that no longer owns anything.
	if err := f.n.Ack(ctx, lease.ID); err == nil {
		t.Fatal("acknowledging an expired lease was accepted")
	}
}

// An agent that crashed after acking and retries on restart should not be told
// it did something wrong.
func TestAckIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("first Ack: %v", err)
	}
	before := f.reload(t, task.ID).ActiveMS

	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("second Ack: %v", err)
	}
	if after := f.reload(t, task.ID).ActiveMS; after != before {
		t.Errorf("acking twice counted the work twice: %d then %d", before, after)
	}
}

// ── batching ──────────────────────────────────────────────────────────────

func TestBatchIsBoundedByMaxItems(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// reviewer batches. Give it more work than its policy allows in one bite:
	// an unbounded batch starves a higher-priority item that arrives late.
	reviewer := roleFromTeam(t, f, "reviewer")
	if reviewer.BatchMaxItems != 8 {
		t.Fatalf("fixture assumes a batch cap of 8, got %d", reviewer.BatchMaxItems)
	}
	for i := 0; i < 12; i++ {
		if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
			To: "reviewer", Commit: "aaaaaaaaaa", Body: "item",
		}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	lease, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(lease.Items) != 8 {
		t.Errorf("batch took %d items, want the cap of 8", len(lease.Items))
	}

	// The remainder is still queued, not lost.
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	next, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || next == nil {
		t.Fatalf("second Claim: %v", err)
	}
	if len(next.Items) != 4 {
		t.Errorf("second batch has %d items, want the remaining 4", len(next.Items))
	}
}

func TestBatchStopsAtAPriorityChange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	for _, p := range []int{10, 10, 50} {
		if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
			To: "reviewer", Commit: "aaaaaaaaaa", Priority: p,
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	lease, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(lease.Items) != 2 {
		t.Fatalf("batch took %d items, want the 2 sharing the head priority", len(lease.Items))
	}
	for _, m := range lease.Items {
		if m.Priority != 10 {
			t.Errorf("batch mixed priority %d with the head's 10", m.Priority)
		}
	}
}

// A task role takes exactly one unit even when more is waiting.
func TestTaskModeTakesOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	for i := 0; i < 3; i++ {
		if _, err := f.n.Send(ctx, f.project.ID, "planner", SendRequest{
			To: "coder", Kind: store.KindNote, Body: "note",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	lease, err := f.n.Claim(ctx, f.project.ID, "coder")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(lease.Items) != 1 {
		t.Errorf("a task-mode role took %d items, want 1", len(lease.Items))
	}
}

// ── approvals ─────────────────────────────────────────────────────────────

func TestRejectReturnsTheCardToItsAuthor(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || lease == nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := f.n.Send(ctx, f.project.ID, "planner", SendRequest{
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.n.Ack(ctx, lease.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pending, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("approvals: %v (%d)", err, len(pending))
	}
	if err := f.n.Reject(ctx, pending[0].ID, "the spec skips error cases"); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if got := f.reload(t, task.ID); got.Lane != "planner" {
		t.Errorf("rejected card sits in %s, want back with planner", got.Lane)
	}
	// Nothing downstream ever saw it.
	if l, err := f.n.Claim(ctx, f.project.ID, "coder"); err != nil {
		t.Fatalf("coder Claim: %v", err)
	} else if l != nil {
		t.Fatal("coder received a rejected handoff")
	}
}

func TestDecidingTwiceIsRejected(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	if _, err := f.n.Send(ctx, f.project.ID, "planner", SendRequest{
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err := f.n.Approve(ctx, pending[0].ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := f.n.Approve(ctx, pending[0].ID); err == nil {
		t.Error("an approval was decided twice")
	}
}

// ── guard rails ───────────────────────────────────────────────────────────

func TestOnlyTheTerminalRoleMayFinishATask(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	_, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, Commit: "aaaaaaaaaa"})
	if err == nil {
		t.Fatal("a mid-pipeline role finished a task")
	}
	if len(f.git.merges) != 0 {
		t.Error("a rejected completion still merged to the base branch")
	}
}

func TestHandoffRequiresACommit(t *testing.T) {
	f := newFixture(t)
	_, err := f.n.Send(context.Background(), f.project.ID, "coder", SendRequest{To: "reviewer"})
	if err == nil {
		t.Fatal("a handoff without a commit was accepted")
	}
}

func TestSendToAnUnknownRoleIsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := f.n.Send(context.Background(), f.project.ID, "coder", SendRequest{
		To: "nobody", Commit: "aaaaaaaaaa",
	})
	if err == nil {
		t.Fatal("a handoff to a role outside the team was accepted")
	}
}

// A failed integration must leave the card where it was: the board claiming
// success over a branch that never moved is worse than a visible error.
func TestFailedMergeLeavesTheCardOpen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.git.err = errors.New("conflict")

	task := f.task(t, "Calculator")
	_, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: task.ID, Commit: "cccccccccc"})
	if err == nil {
		t.Fatal("completion succeeded despite a failed merge")
	}
	if got := f.reload(t, task.ID); got.State == store.TaskDone {
		t.Error("the card was closed even though integration failed")
	}
}

func roleFromTeam(t *testing.T, f *fixture, name string) store.ResolvedRole {
	t.Helper()
	team, err := f.db.ResolveTeam(context.Background(), f.project.ID)
	if err != nil {
		t.Fatalf("ResolveTeam: %v", err)
	}
	for _, r := range team {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("role %q is not on the team", name)
	return store.ResolvedRole{}
}
