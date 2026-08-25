package nydus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/konfessor/zerg/internal/hatchery"
	"github.com/konfessor/zerg/internal/store"
)

// fakeIntegrator records merges so a test can assert integration happened
// without needing a repository.
type fakeIntegrator struct {
	mu        sync.Mutex
	merges    []string
	into      []string
	published []string
	err       error
}

// Resolve is identity here: these tests pass shas already, and the point of
// the real one is the tree it resolves against, which a fake has none of.
// Publish records the request and returns a plausible URL, so a test can check
// that PR mode published without needing a remote or a GitHub account.
func (f *fakeIntegrator) Publish(_ context.Context, _, base, commit, title, body string) (string, error) {
	f.published = append(f.published, title+" -> "+base+"@"+commit[:min(8, len(commit))])
	_ = body
	return "https://example.test/pr/1", nil
}

func (f *fakeIntegrator) Resolve(_ context.Context, _, ref string) (string, error) {
	return ref, nil
}

func (f *fakeIntegrator) MergeInto(_ context.Context, _, commit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.into = append(f.into, commit)
	return nil
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
	p, err := db.CreateProject(ctx, mustDir(t, "repo"), "", "")
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
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa", Body: "handed on"}); err != nil {
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
		TaskID: task.ID, To: "reviewer", Commit: "bbbbbbbbbb", Body: "handed on"}); err != nil {
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
		TaskID: task.ID, Commit: "cccccccccc", Body: "handed on"}); err != nil {
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

// Two claimers must never receive the same work. List a directory and then move
// a file and the loser throws — or, in batch mode, the queue splits into two
// directories with no recovery path.
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

// The permanent stall, made recoverable: an agent that dies holding work must
// not take the work with it.
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
			To: "reviewer", Commit: "aaaaaaaaaa", Priority: p, Body: "handed on"}); err != nil {
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
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa", Body: "handed on"}); err != nil {
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
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa", Body: "handed on"}); err != nil {
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

	_, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "handed on"})
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
		To: "nobody", Commit: "aaaaaaaaaa", Body: "handed on"})
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
	_, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{TaskID: task.ID, Commit: "cccccccccc", Body: "handed on"})
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

// A hand-off has to arrive in the recipient's tree. The first version of this
// system told agents it had merged for them while nothing merged anything, and
// the unit test agreed, because both read the same field. A live reviewer found
// its worktree missing the code twice before concluding it was being lied to.
//
// So this one uses a real repository and a real integrator, and asks git.
func TestClaimMergesHandoffIntoWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)

	hat := hatchery.New(repo)
	for _, role := range []string{"coder", "reviewer"} {
		if _, err := hat.EnsureWorktree(ctx, role, "main"); err != nil {
			t.Fatalf("worktree for %s: %v", role, err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	project, err := db.CreateProject(ctx, repo, "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tpl := func(name string) string {
		x, err := db.GetTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("GetTemplateByName(%q): %v", name, err)
		}
		return x.ID
	}
	if err := db.SetTeam(ctx, project.ID, []store.ProjectRole{
		{TemplateID: tpl("coder"), Enabled: true},
		{TemplateID: tpl("reviewer"), Enabled: true},
	}); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	n := New(db, WithIntegrator(Git{}))
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// The coder commits in its own worktree, exactly as a real one does.
	coderTree := hat.Path("coder")
	write(t, coderTree, "calc.rs", "fn eval() {}")
	sha := commitAll(t, coderTree, "implement eval")

	if _, err := n.Send(ctx, project.ID, "coder", SendRequest{
		To: "reviewer", TaskID: task.ID, Commit: sha, Body: "implemented",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	lease, err := n.Claim(ctx, project.ID, "reviewer")
	if err != nil || lease == nil {
		t.Fatalf("claim: lease=%v err=%v", lease, err)
	}
	if !lease.Merged[lease.Items[0].ID] {
		t.Error("claim reported the hand-off as unmerged")
	}

	// The check that matters: ask git, not the struct that just told us.
	reviewerTree := hat.Path("reviewer")
	if _, err := runGit(ctx, reviewerTree, "merge-base", "--is-ancestor", sha, "HEAD"); err != nil {
		t.Errorf("commit %s is not in the reviewer's worktree: %v", sha[:8], err)
	}
	got, err := os.ReadFile(filepath.Join(reviewerTree, "calc.rs"))
	if err != nil || string(got) != "fn eval() {}" {
		t.Errorf("reviewer's tree: calc.rs = %q, err = %v; the work did not arrive", got, err)
	}
}

// setupRealRepo builds a project on a real repository with real worktrees and a
// real integrator, which is the only configuration where ref resolution and
// merging can be checked against git rather than against a double.
func setupRealRepo(t *testing.T, roles ...string) (*Nydus, *store.Project, *hatchery.Hatchery) {
	t.Helper()
	ctx := context.Background()
	repo := newRepo(t)
	hat := hatchery.New(repo)
	for _, role := range roles {
		if _, err := hat.EnsureWorktree(ctx, role, "main"); err != nil {
			t.Fatalf("worktree for %s: %v", role, err)
		}
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	project, err := db.CreateProject(ctx, repo, "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	team := make([]store.ProjectRole, 0, len(roles))
	for _, role := range roles {
		tpl, err := db.GetTemplateByName(ctx, role)
		if err != nil {
			t.Fatalf("GetTemplateByName(%q): %v", role, err)
		}
		team = append(team, store.ProjectRole{TemplateID: tpl.ID, Enabled: true})
	}
	if err := db.SetTeam(ctx, project.ID, team); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}
	return New(db, WithIntegrator(Git{})), project, hat
}

// Agents write `--commit HEAD`, and HEAD means the tip of whichever tree
// resolves it. Stored unresolved, the coder's "my new commit" becomes "main's
// tip" the moment the orchestrator reads it at the project root — where
// `merge --ff-only HEAD` is a no-op that reports success.
//
// That shipped once: a task went to Done with the base branch still on its
// initial commit. The ref must be pinned in the sender's tree.
func TestSendResolvesHeadInTheSendersWorktree(t *testing.T) {
	ctx := context.Background()
	n, project, hat := setupRealRepo(t, "coder", "reviewer")
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	coderTree := hat.Path("coder")
	write(t, coderTree, "calc.rs", "fn eval() {}")
	want := commitAll(t, coderTree, "implement eval")

	msg, err := n.Send(ctx, project.ID, "coder", SendRequest{
		To: "reviewer", TaskID: task.ID, Commit: "HEAD", Body: "implemented",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if msg.CommitSHA == nil {
		t.Fatal("the handoff carries no commit")
	}
	if *msg.CommitSHA == "HEAD" {
		t.Fatal("stored the literal ref; it means a different commit in every tree")
	}
	if *msg.CommitSHA != want {
		t.Errorf("stored %s, want the coder's tip %s", *msg.CommitSHA, want)
	}

	// And the distinction that matters: it is not the project root's HEAD.
	mainHead, err := Git{}.Resolve(ctx, project.Path, "HEAD")
	if err != nil {
		t.Fatalf("resolving main: %v", err)
	}
	if *msg.CommitSHA == mainHead {
		t.Error("resolved against the project root, not the sender's worktree")
	}
}

// Completion is the hand-off that moves the project's own branch. The first
// run reported a task done while main stayed on its initial commit, so this
// asserts the branch, not the card.
func TestCompleteIntegratesIntoTheBaseBranch(t *testing.T) {
	ctx := context.Background()
	n, project, hat := setupRealRepo(t, "coder")
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	coderTree := hat.Path("coder")
	write(t, coderTree, "calc.rs", "fn eval() {}")
	want := commitAll(t, coderTree, "implement eval")

	// One enabled role is terminal, so it finishes the task itself.
	if _, err := n.Send(ctx, project.ID, "coder", SendRequest{
		TaskID: task.ID, Commit: "HEAD", Body: "done",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := Git{}.Resolve(ctx, project.Path, "main")
	if err != nil {
		t.Fatalf("resolving main: %v", err)
	}
	if got != want {
		t.Errorf("main is at %s, want %s; the task finished over a branch that never moved", got[:8], want[:8])
	}
	if _, err := os.Stat(filepath.Join(project.Path, "calc.rs")); err != nil {
		t.Errorf("the work is not on the base branch: %v", err)
	}
}

// Nothing to integrate is not a completion. The guard used to sit around the
// merge alone, so an empty commit meant "merge nothing, mark it done".
func TestCompleteRefusesWithoutACommit(t *testing.T) {
	ctx := context.Background()
	n, project, _ := setupRealRepo(t, "coder")
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if _, err := n.Send(ctx, project.ID, "coder", SendRequest{
		TaskID: task.ID, Body: "done", Kind: store.KindNote,
	}); err == nil {
		t.Error("completed a task with no commit to integrate")
	}
}

// A lease outlives the process that held it. Agents are children of the daemon,
// so a restart leaves open leases with no holder — and because the route is
// `claimed` rather than `queued`, nothing nudges anyone and the work sits until
// the deadline. Observed: a card reading "working" with twenty minutes left on
// a lease, beside two live agents that had been handed nothing.
func TestReclaimRequeuesLeasesThatOutlivedTheirHolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, WithLease(time.Hour)) // nowhere near expiry
	task := f.task(t, "Calculator")

	// From coder, not planner: the planner's handoffs sit behind an approval
	// gate, so they are held rather than queued and there is nothing to claim.
	if _, err := f.n.Send(ctx, f.project.ID, "coder", SendRequest{
		To: "reviewer", TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "implemented",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	lease, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil || lease == nil {
		t.Fatalf("claim: %v %v", lease, err)
	}

	// The holder dies with the daemon. Expiry alone does nothing here.
	if n, err := f.n.ExpireLeases(ctx); err != nil || n != 0 {
		t.Fatalf("expiry took %d leases before the deadline (err %v); it must not", n, err)
	}

	n, err := f.n.ReclaimOrphanedLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimOrphanedLeases: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d leases, want the 1 that was open", n)
	}

	// The proof is that the work is claimable again, not that a counter moved.
	again, err := f.n.Claim(ctx, f.project.ID, "reviewer")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if again == nil {
		t.Fatal("the work did not return to the queue; it is stranded until the deadline")
	}
	if again.ID == lease.ID {
		t.Error("the same dead lease was handed back rather than a new one")
	}
}

// How finished work lands is the project's decision. Merging to the base is
// right for a repository you own and wrong wherever the base is protected, so
// all three outcomes have to be reachable — and only the chosen one may happen.
func TestIntegrationModeDecidesWhatCompletionDoes(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		mode       string
		wantMerges int
		wantPRs    int
		wantInBody string
	}{
		{store.IntegrateMerge, 1, 0, ""},
		{store.IntegrateBranch, 0, 0, ""},
		{store.IntegratePR, 0, 1, "Pull request: https://example.test/pr/1"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.db.SetIntegration(ctx, f.project.ID, tc.mode); err != nil {
				t.Fatalf("SetIntegration: %v", err)
			}
			task := f.task(t, "Calculator")

			// reviewer is terminal in this fixture, so it finishes the task.
			msg, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
				TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved",
			})
			if err != nil {
				t.Fatalf("complete: %v", err)
			}

			if got := len(f.git.merges); got != tc.wantMerges {
				t.Errorf("%s: %d merges to the base branch, want %d", tc.mode, got, tc.wantMerges)
			}
			if got := len(f.git.published); got != tc.wantPRs {
				t.Errorf("%s: %d pull requests, want %d", tc.mode, got, tc.wantPRs)
			}
			if tc.wantInBody != "" && !strings.Contains(msg.Body, tc.wantInBody) {
				t.Errorf("%s: the completion does not record where the work went: %q", tc.mode, msg.Body)
			}
			// The task is finished either way. Whether it has landed is a
			// separate question from whether the work is done.
			if f.reload(t, task.ID).State != store.TaskDone {
				t.Errorf("%s: the task did not reach Done", tc.mode)
			}
		})
	}
}

// mustDir makes a directory for a project to point at. Paths are validated on
// creation now, so a test cannot name one that does not exist.
func mustDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	return dir
}
