package nydus

import (
	"context"
	"errors"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIntegrator records merges so a test can assert integration happened
// without needing a repository.
type fakeIntegrator struct {
	mu             sync.Mutex
	merges         []string
	into           []string
	published      []string
	publishedDraft []bool
	// publishedURL is what each title's pull request answers to, so Landed can
	// report where the work went the way `gh` does.
	publishedURL map[string]string
	err          error
	// landedErr makes the "did this integration finish" question unanswerable,
	// which must leave an interrupted approval claimed rather than releasing
	// one whose merge may have happened.
	landedErr error

	// enter is closed as Merge begins and hold blocks it until released.
	// Integration runs outside the write transaction — a git subprocess must
	// never hold the single writer — and that gap is where a second decision
	// can slip in. Without a way to widen it deterministically, a race test
	// passes against unguarded code because a fake merge returns in
	// nanoseconds.
	enter chan struct{}
	hold  chan struct{}
	// once, so the fields are written before the goroutines start and never
	// mutated afterwards — a fake that nils its own channel to fire once is a
	// data race with the test reading it.
	once sync.Once
}

// Resolve is identity here: these tests pass shas already, and the point of
// the real one is the tree it resolves against, which a fake has none of.
// Publish records the request and returns a plausible URL, so a test can check
// that PR mode published without needing a remote or a GitHub account.
func (f *fakeIntegrator) Publish(_ context.Context, _, base, commit, title, body string, draft bool) (string, error) {
	f.published = append(f.published, title+" -> "+base+"@"+commit[:min(8, len(commit))])
	f.publishedDraft = append(f.publishedDraft, draft)
	_ = body
	url := "https://example.test/pr/1"
	if f.publishedURL == nil {
		f.publishedURL = map[string]string{}
	}
	f.publishedURL[title] = url
	return url, nil
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

// Landed answers from what Merge and Publish recorded, so a test can replay a
// crash mid-integration without a git repository.
func (f *fakeIntegrator) Landed(_ context.Context, _, _, commit, title, mode string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.landedErr != nil {
		return "", false, f.landedErr
	}
	switch mode {
	case "branch":
		return "", false, nil
	case "pr":
		// The URL comes back with the answer, the way `gh` reports it: a gated
		// completion's note was written before anything was published, so this
		// is the only place a recovered card can learn where its work went.
		if url := f.publishedURL[title]; url != "" {
			return url, true, nil
		}
		return "", false, nil
	default:
		for _, c := range f.merges {
			if c == commit {
				return commit, true, nil
			}
		}
		return "", false, nil
	}
}

func (f *fakeIntegrator) Merge(_ context.Context, _, _, commit string) error {
	if f.enter != nil {
		f.once.Do(func() { close(f.enter) })
		<-f.hold
	}
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
	if err := db.SetTeam(ctx, p.ID, []store.TeamPresetRole{
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
	task, err := f.n.NewTask(context.Background(), f.project.ID, name, "do the thing", "")
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
	if err := db.SetTeam(ctx, project.ID, []store.TeamPresetRole{
		{TemplateID: tpl("coder"), Enabled: true},
		{TemplateID: tpl("reviewer"), Enabled: true},
	}); err != nil {
		t.Fatalf("SetTeam: %v", err)
	}

	n := New(db, WithIntegrator(Git{}))
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing", "")
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
	team := make([]store.TeamPresetRole, 0, len(roles))
	for _, role := range roles {
		tpl, err := db.GetTemplateByName(ctx, role)
		if err != nil {
			t.Fatalf("GetTemplateByName(%q): %v", role, err)
		}
		team = append(team, store.TeamPresetRole{TemplateID: tpl.ID, Enabled: true})
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
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing", "")
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
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing", "")
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
	task, err := n.NewTask(ctx, project.ID, "Calculator", "do the thing", "")
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
		name        string
		mode        string
		draft       bool
		wantMerges  int
		wantPRs     int
		wantInBody  string
		wantOutcome string
		wantRef     string
	}{
		{"merge", store.IntegrateMerge, false, 1, 0, "", store.OutcomeMerged, "aaaaaaaaaa"},
		{"branch", store.IntegrateBranch, false, 0, 0, "", store.OutcomeBranch, "aaaaaaaaaa"},
		{"pr", store.IntegratePR, false, 0, 1, "Pull request: https://example.test/pr/1", store.OutcomePR, "https://example.test/pr/1"},
		{"draft-pr", store.IntegratePR, true, 0, 1, "Pull request: https://example.test/pr/1", store.OutcomePR, "https://example.test/pr/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.db.SetIntegration(ctx, f.project.ID, tc.mode, tc.draft); err != nil {
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
			if tc.wantPRs == 1 && f.git.publishedDraft[0] != tc.draft {
				t.Errorf("%s: published draft=%v, want %v", tc.name, f.git.publishedDraft[0], tc.draft)
			}
			if tc.wantInBody != "" && !strings.Contains(msg.Body, tc.wantInBody) {
				t.Errorf("%s: the completion does not record where the work went: %q", tc.mode, msg.Body)
			}

			// What happened is stored on the card, not left to be read back out
			// of that sentence or guessed from the project's setting, which is
			// a live value and answers differently the moment it is changed.
			done, err := f.db.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if done.Outcome != tc.wantOutcome || done.OutcomeRef != tc.wantRef {
				t.Errorf("%s: recorded outcome %q %q, want %q %q",
					tc.mode, done.Outcome, done.OutcomeRef, tc.wantOutcome, tc.wantRef)
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

// A terminal role with an approval gate must not reach the base branch until a
// human says so. The gate used to be applied only to routed hand-offs, and
// completion returned before reaching it — so a reviewer configured to require
// approval merged without asking, which is the setting doing the opposite of
// what it says, to the one action that changes the repository.
func TestGatedTerminalRoleWaitsForApprovalBeforeLanding(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// reviewer is terminal in this fixture; give it a gate.
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatalf("gating the reviewer: %v", err)
	}
	task := f.task(t, "Calculator")

	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved by the reviewer",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Nothing has landed, and the card is not Done — it is finished by the
	// roles and not yet landed, which is a real state.
	if len(f.git.merges) != 0 {
		t.Fatalf("merged %v before anyone approved", f.git.merges)
	}
	if got := f.reload(t, task.ID).State; got == store.TaskDone {
		t.Error("the board says Done over work nobody has approved")
	}

	pending, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d approvals waiting, want 1", len(pending))
	}
	// The card has to say what is being approved, or it is asking someone to
	// decide about something they cannot read.
	if pending[0].Body != "approved by the reviewer" {
		t.Errorf("approval carries body %q; the decision needs the note", pending[0].Body)
	}

	if err := f.n.Approve(ctx, pending[0].ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(f.git.merges) != 1 {
		t.Errorf("approving did not land the work: %v merges", len(f.git.merges))
	}
	// Landing through an approval records what happened, the same as landing
	// without one: a history that reads outcomes cannot have a hole where the
	// gated pipelines are.
	if done := f.reload(t, task.ID); done.Outcome != store.OutcomeMerged || done.OutcomeRef != "aaaaaaaaaa" {
		t.Errorf("approved completion recorded %q %q, want merged aaaaaaaaaa", done.Outcome, done.OutcomeRef)
	}
	if got := f.reload(t, task.ID).State; got != store.TaskDone {
		t.Errorf("task state is %q after approval, want done", got)
	}
}

// Rejecting a completion must land nothing and give the work back.
func TestRejectedCompletionLandsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")

	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved by the reviewer",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err := f.n.Reject(ctx, pending[0].ID, "not yet"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if len(f.git.merges) != 0 {
		t.Errorf("a rejected completion still merged: %v", f.git.merges)
	}
	if got := f.reload(t, task.ID).State; got == store.TaskDone {
		t.Error("a rejected completion left the card Done")
	}
}

// Approving a terminal handoff merges before the decision is recorded, and the
// merge cannot hold the write transaction — a git subprocess must never hold
// the single writer. That gap used to be unguarded: two decisions could both
// read pending, both integrate, and the later overwrite the earlier. An approve
// racing a reject recorded "rejected" over a branch that had already landed.
func TestOnlyOneDecisionSurvivesARace(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatalf("gating the reviewer: %v", err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "done",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if len(pending) != 1 {
		t.Fatalf("got %d approvals, want 1", len(pending))
	}
	id := pending[0].ID

	// Hold the approver inside the merge, which is precisely the window the
	// transaction is not covering, and let the rejecter run while it is open.
	f.git.enter = make(chan struct{})
	f.git.hold = make(chan struct{})

	approveErr := make(chan error, 1)
	go func() { approveErr <- f.n.Approve(ctx, id) }()

	<-f.git.enter // the merge has begun and has not returned
	rejectErr := f.n.Reject(ctx, id, "no")
	close(f.git.hold)
	approve := <-approveErr

	if approve != nil {
		t.Errorf("the decision that got there first failed: %v", approve)
	}
	if rejectErr == nil {
		t.Error("a second decision succeeded while the first was mid-merge")
	}

	// The record must agree with what happened to the branch.
	a, err := f.db.GetApproval(ctx, id)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	f.git.mu.Lock()
	merged := len(f.git.merges) > 0
	f.git.mu.Unlock()

	if a.State != store.ApprovalApproved {
		t.Errorf("approval recorded as %q, want approved", a.State)
	}
	if !merged {
		t.Error("recorded a decision with nothing merged")
	}
}

// ── crash recovery ────────────────────────────────────────────────────────

// A daemon killed between claiming an approval and recording it leaves the
// approval "integrating": Attention excludes it, so nobody is asked about it,
// and the work may already be on the base branch with a card that never moved.
func TestInterruptedApprovalIsSettledFromTheRepository(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved by the reviewer",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)

	// The crash: claimed, merged, and killed before the decision was written.
	f.stick(t, pending[0].ID)
	f.git.mu.Lock()
	f.git.merges = append(f.git.merges, "aaaaaaaaaa")
	f.git.mu.Unlock()

	settled, released, err := f.n.ReconcileIntegrating(ctx)
	if err != nil {
		t.Fatalf("ReconcileIntegrating: %v", err)
	}
	if settled != 1 || released != 0 {
		t.Fatalf("settled %d and released %d; the merge had happened, so it should be settled", settled, released)
	}
	if got := f.reload(t, task.ID).State; got != store.TaskDone {
		t.Errorf("task state is %q; the work is on the base branch", got)
	}
	a, err := f.db.GetApproval(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.ApprovalApproved {
		t.Errorf("approval state is %q, want approved", a.State)
	}
	// The reconciler is the third way a card can end, and it knows the mode it
	// just asked git about, so it records the outcome too rather than leaving
	// the one card that survived a crash blank.
	if done := f.reload(t, task.ID); done.Outcome != store.OutcomeMerged || done.OutcomeRef != "aaaaaaaaaa" {
		t.Errorf("settled card recorded %q %q, want merged aaaaaaaaaa", done.Outcome, done.OutcomeRef)
	}
}

// And when the merge had not happened, the approval goes back to the operator
// rather than staying invisible.
func TestInterruptedApprovalThatNeverLandedGoesBackToPending(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved by the reviewer",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.stick(t, pending[0].ID)

	settled, released, err := f.n.ReconcileIntegrating(ctx)
	if err != nil {
		t.Fatalf("ReconcileIntegrating: %v", err)
	}
	if settled != 0 || released != 1 {
		t.Fatalf("settled %d and released %d; nothing had landed", settled, released)
	}
	back, err := f.db.ListPendingApprovals(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 {
		t.Fatalf("%d approvals waiting for a person, want 1", len(back))
	}
}

// An unanswerable repository leaves the claim in place: releasing an approval
// whose merge may have happened is how the same work lands twice.
func TestInterruptedApprovalStaysClaimedWhenTheRepositoryCannotSay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved by the reviewer",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	f.stick(t, pending[0].ID)
	f.git.mu.Lock()
	f.git.landedErr = errors.New("git is not answering")
	f.git.mu.Unlock()

	if _, _, err := f.n.ReconcileIntegrating(ctx); err == nil {
		t.Fatal("an unanswerable repository was treated as an answer")
	}
	if back, _ := f.db.ListPendingApprovals(ctx, f.project.ID); len(back) != 0 {
		t.Error("the approval was released while it was still unknown whether it had landed")
	}
}

// stick puts an approval into the state a crash mid-decision leaves behind.
func (f *fixture) stick(t *testing.T, approvalID string) {
	t.Helper()
	if _, err := f.db.SQL().ExecContext(context.Background(),
		`UPDATE approvals SET state = ? WHERE id = ?`,
		store.ApprovalIntegrating, approvalID); err != nil {
		t.Fatalf("simulating the crash: %v", err)
	}
}

// An event's task is the one its role held when the event happened, not the one
// it holds by the time the recorder gets to it.
//
// The recorder writes behind a queue, so those are different questions under
// load: a burst emitted while coder held task A can reach the writer after
// coder has claimed task B, and asking for the newest lease moves A's tokens
// and A's transcript onto B — silently, and worst for the most expensive turns.
func TestTaskAttributionUsesTheLeaseHeldAtTheTime(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	first := f.task(t, "First")
	leaseA, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || leaseA == nil {
		t.Fatalf("claiming the first task: %v", err)
	}
	duringA := f.clock.now()
	if err := f.n.Ack(ctx, leaseA.ID); err != nil {
		t.Fatal(err)
	}

	// Time passes, and the same role takes the next card.
	f.clock.advance(time.Minute)
	second := f.task(t, "Second")
	leaseB, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || leaseB == nil {
		t.Fatalf("claiming the second task: %v", err)
	}

	at, err := f.db.TaskForAt(ctx, f.project.ID, "planner", duringA)
	if err != nil {
		t.Fatalf("TaskForAt: %v", err)
	}
	if at == nil || *at != first.ID {
		t.Errorf("an event from the first card was attributed to %v, want %s", at, first.ID)
	}

	now, err := f.db.CurrentTaskFor(ctx, f.project.ID, "planner")
	if err != nil {
		t.Fatalf("CurrentTaskFor: %v", err)
	}
	if now == nil || *now != second.ID {
		t.Errorf("the role's current card is %v, want %s", now, second.ID)
	}
}

// An integration that cannot run must say why, in the cockpit.
//
// These are the operator's to fix and each states how — no remote, no gh, the
// wrong branch checked out, a base that has moved. Left as plain errors they
// reach the API as a 500 and render as "internal error", so the sentence that
// says what to do never arrives. Observed on a repository with no remote:
// Approve failed six times in a row and said nothing either time.
func TestIntegrationFailuresAreReportedToTheOperator(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	git := Git{}

	// Not a repository at all, so the branch check fails first.
	err := git.Merge(ctx, dir, "main", "deadbeef")
	if err == nil {
		t.Fatal("merging into a non-repository reported success")
	}

	// A repository with no remote cannot open a pull request, and says so.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init")

	_, err = git.Publish(ctx, dir, "main", "HEAD", "A task", "body", false)
	if err == nil {
		t.Fatal("publishing with no remote reported success")
	}
	var v interface{ Validation() }
	if !errors.As(err, &v) {
		t.Errorf("%q is not marked as the caller's to fix, so the API hides it behind \"internal error\"", err)
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Errorf("the message does not name the problem: %v", err)
	}
}

// A stopped card stays stopped, even when the role holding it was mid-turn.
//
// Stopping closes routes and releases the lease, but neither reaches an agent
// that is already working: it finishes, sends, and the send used to set the
// card back to queued and walk it to the next role. The operator's stop had
// stopped nothing.
func TestStoppedCardRefusesLateHandoffs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	task := f.task(t, "Graph function")
	lease, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil || lease == nil {
		t.Fatalf("planner Claim: %v (lease %v)", err, lease)
	}

	// The operator stops it while planner is still running.
	if err := f.db.StopTask(ctx, f.project.ID, task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if got := f.reload(t, task.ID); got.StoppedAt == nil {
		t.Fatal("stopped_at is unset, and a person parking a card must be distinguishable " +
			"from a role rejecting it, and both leave state \"rejected\"")
	}

	// planner finishes its turn and hands off, none the wiser.
	_, err = f.n.Send(ctx, f.project.ID, "planner", SendRequest{
		TaskID: task.ID, To: "coder", Commit: "aaaaaaaaaa", Body: "done anyway"})
	if err == nil {
		t.Fatal("a handoff for a stopped card was accepted; the stop did nothing")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error = %v, want it to say the card was stopped", err)
	}

	got := f.reload(t, task.ID)
	if got.State != store.TaskRejected || got.StoppedAt == nil {
		t.Errorf("state = %q stopped_at = %v after a late handoff, want it still stopped",
			got.State, got.StoppedAt)
	}
	if got.Lane != "planner" {
		t.Errorf("lane = %q; a stopped card must not walk to the next role", got.Lane)
	}
}

// The same guard on the terminal path: finishing a stopped card would merge
// work the operator had already called off.
func TestStoppedCardRefusesLateCompletion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	task := f.task(t, "Graph function")
	if _, err := f.n.Claim(ctx, f.project.ID, "planner"); err != nil {
		t.Fatalf("planner Claim: %v", err)
	}
	if err := f.db.StopTask(ctx, f.project.ID, task.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	// reviewer is the terminal role, so this is the merge path.
	_, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "bbbbbbbbbb", Body: "shipping it"})
	if err == nil {
		t.Fatal("a stopped card was completed and merged")
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error = %v, want it to say the card was stopped", err)
	}
	if got := f.reload(t, task.ID); got.State != store.TaskRejected || got.StoppedAt == nil {
		t.Errorf("state = %q stopped_at = %v, want it still stopped", got.State, got.StoppedAt)
	}
}

// A gated pull request keeps its URL across a crash.
//
// The note a completion carries is written when the role finishes, and a gated
// one is not published until somebody approves it, so the note cannot name the
// pull request: it did not exist yet. The process that opened it is the one
// that was killed. git is the only thing that still knows, which is why Landed
// answers with the URL and not only with the fact.
func TestInterruptedGatedPullRequestKeepsItsURL(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SetIntegration(ctx, f.project.ID, store.IntegratePR, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	msg, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "looks right",
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// The note was written before anyone approved it, so it cannot name a pull
	// request. This is the fact the recovery has to work around.
	if strings.Contains(msg.Body, "Pull request") {
		t.Fatalf("a held completion already names a pull request: %q", msg.Body)
	}

	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if len(pending) != 1 {
		t.Fatalf("got %d approvals waiting, want 1", len(pending))
	}
	// The crash: claimed, published, killed before the decision was written.
	f.stick(t, pending[0].ID)
	if _, err := f.git.Publish(ctx, f.project.Path, "main", "aaaaaaaaaa", task.Name, "looks right", false); err != nil {
		t.Fatal(err)
	}

	if settled, released, err := f.n.ReconcileIntegrating(ctx); err != nil || settled != 1 || released != 0 {
		t.Fatalf("reconciling: settled %d released %d (%v), want it settled", settled, released, err)
	}

	done := f.reload(t, task.ID)
	if done.Outcome != store.OutcomePR {
		t.Errorf("recovered card records outcome %q, want a pull request", done.Outcome)
	}
	if done.OutcomeRef != "https://example.test/pr/1" {
		t.Errorf("recovered card points at %q; the pull request it opened is unreachable", done.OutcomeRef)
	}
}

// Work does not merge with a question still open on it.
//
// That is the whole reason a review remark is a row with a state rather than a
// note in a body: the gate can ask. Rejecting is not refused, because rejecting
// is the answer to those threads.
func TestApprovingIsRefusedWhileAReviewThreadIsOpen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "ready",
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if len(pending) != 1 {
		t.Fatalf("got %d approvals, want 1", len(pending))
	}

	thread, err := f.db.OpenReviewThread(ctx, &store.ReviewThread{
		ProjectID: f.project.ID, TaskID: task.ID, File: "src/parse.rs", Line: 41,
	}, store.OperatorRole, "why is this recursive?")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.n.Approve(ctx, pending[0].ID); err == nil {
		t.Fatal("approved over an open question")
	} else if !strings.Contains(err.Error(), "review thread") {
		t.Errorf("refusal is %q; it has to say what is in the way", err)
	}
	if len(f.git.merges) != 0 {
		t.Errorf("merged %v with a thread open", f.git.merges)
	}
	if got := f.reload(t, task.ID).State; got == store.TaskDone {
		t.Error("the card says done over an unanswered question")
	}

	// Settling it lets the decision through.
	if err := f.db.SetReviewThreadState(ctx, thread.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.n.Approve(ctx, pending[0].ID); err != nil {
		t.Fatalf("approving a settled review: %v", err)
	}
	if len(f.git.merges) != 1 {
		t.Errorf("approving after settling did not land the work: %d merges", len(f.git.merges))
	}
}

// Rejecting is how those threads get answered, so it is never in their way.
func TestRejectingIsAllowedWithThreadsOpen(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "Calculator")
	if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
		TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)
	if _, err := f.db.OpenReviewThread(ctx, &store.ReviewThread{
		ProjectID: f.project.ID, TaskID: task.ID, File: "src/parse.rs", Line: 41,
	}, store.OperatorRole, "this needs a test"); err != nil {
		t.Fatal(err)
	}

	if err := f.n.Reject(ctx, pending[0].ID, "see the comments"); err != nil {
		t.Fatalf("rejecting with threads open: %v", err)
	}
	// The card goes back to whoever wrote it, with the threads still on it.
	if n, _ := f.db.OpenReviewThreads(ctx, task.ID); n != 1 {
		t.Errorf("%d open threads after rejecting, want the one that caused it", n)
	}
}

// A rejection has to reach the role that has to act on it.
//
// It did not. The reason was written on the approval row, which no agent
// reads, the remarks stayed in the review tables, and the card's lane changed
// with nothing queued behind it -- so the author had nothing to claim and a
// rejected card sat in their column for ever.
func TestRejectingHandsTheReviewBackToTheAuthor(t *testing.T) {
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
	pending, _ := f.db.ListPendingApprovals(ctx, f.project.ID)

	thread, err := f.db.OpenReviewThread(ctx, &store.ReviewThread{
		ProjectID: f.project.ID, TaskID: task.ID, ApprovalID: &pending[0].ID,
		File: "src/parse.rs", Line: 41,
	}, store.OperatorRole, "this needs a test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddReviewComment(ctx, thread.ID, "coder", "which case?"); err != nil {
		t.Fatal(err)
	}

	// Rejecting with nothing typed: the reasons are already on the lines they
	// belong to, which is the whole point of remarking there.
	if err := f.n.Reject(ctx, pending[0].ID, ""); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	back, err := f.n.Claim(ctx, f.project.ID, "planner")
	if err != nil {
		t.Fatalf("author Claim: %v", err)
	}
	if back == nil || len(back.Items) == 0 {
		t.Fatal("the author has nothing to claim after a rejection")
	}
	got := back.Items[0].Body
	for _, want := range []string{"src/parse.rs:41", "this needs a test", "coder: which case?"} {
		if !strings.Contains(got, want) {
			t.Errorf("the returned card does not carry %q:\n%s", want, got)
		}
	}
	// And it is still the same card, not a new one.
	if back.Items[0].TaskID == nil || *back.Items[0].TaskID != task.ID {
		t.Errorf("the rejection came back on %v, want task %s", back.Items[0].TaskID, task.ID)
	}
}

// Where a card gets deployed is decided when it is written, and has to survive
// being written down: the whole point of moving it off the project is that a
// card carries its own answer.
func TestATaskRemembersWhereItDeploys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	task, err := f.n.NewTask(ctx, f.project.ID, "Add the settings page", "do it", store.DeployLocal)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if got := f.reload(t, task.ID).Deploy; got != store.DeployLocal {
		t.Errorf("deploy = %q, want local — the card came back without it", got)
	}

	// The default is nowhere, which is what every card written before this
	// existed asked for by not being able to ask.
	plain := f.task(t, "Fix a typo")
	if got := f.reload(t, plain.ID).Deploy; got != "" {
		t.Errorf("deploy = %q on a card that did not ask, want empty", got)
	}
}

// A target this build cannot reach is refused rather than stored. Stored, it
// would be a card that looks like it deploys and never does, and the person
// who asked would find out by it not happening.
func TestATaskCannotAskToDeploySomewhereThatDoesNotExist(t *testing.T) {
	f := newFixture(t)

	_, err := f.n.NewTask(context.Background(), f.project.ID, "Ship it", "do it", "production")
	if err == nil {
		t.Fatal("a card asked to deploy to production and was accepted")
	}
	var invalid *validationError
	if !errors.As(err, &invalid) {
		t.Errorf("err = %v, want one the API renders as a 400", err)
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("err = %v, want it to name what was asked for", err)
	}
}

// A card that lands reports the commit that landed, on both the path that ends
// in a role finishing and the path that ends in a person approving.
//
// This is the seam the deploy hangs on: the daemon starts a preview here, and
// it starts it *at* a commit. It was wired only to the ungated path once, so
// on any project with an approval gate at the end -- the arrangement the gate
// exists for -- nothing that runs when a card lands ran at all.
func TestLandingReportsTheCommitThatLanded(t *testing.T) {
	ctx := context.Background()

	type landing struct{ projectID, taskID, commit string }

	t.Run("a role finishes it", func(t *testing.T) {
		var landed []landing
		f := newFixture(t, WithOnTaskDone(func(_ context.Context, p, tk, c string) {
			landed = append(landed, landing{p, tk, c})
		}))
		task := f.task(t, "Calculator")

		// reviewer is terminal in this fixture, so this finishes the card.
		if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
			TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "approved"}); err != nil {
			t.Fatalf("complete: %v", err)
		}

		if len(landed) != 1 {
			t.Fatalf("%d landings, want 1", len(landed))
		}
		if landed[0] != (landing{f.project.ID, task.ID, "aaaaaaaaaa"}) {
			t.Errorf("landed %+v, want the project, the card and the commit", landed[0])
		}
	})

	t.Run("a person approves it", func(t *testing.T) {
		var landed []landing
		f := newFixture(t, WithOnTaskDone(func(_ context.Context, p, tk, c string) {
			landed = append(landed, landing{p, tk, c})
		}))
		task := f.task(t, "Calculator")

		// reviewer gates its completion, so finishing it only asks.
		if _, err := f.db.SQL().ExecContext(ctx,
			`UPDATE role_templates SET gate = ? WHERE name = 'reviewer'`, store.GateApproval); err != nil {
			t.Fatalf("gating reviewer: %v", err)
		}
		if _, err := f.n.Send(ctx, f.project.ID, "reviewer", SendRequest{
			TaskID: task.ID, Commit: "bbbbbbbbbb", Body: "approved"}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if len(landed) != 0 {
			t.Fatalf("the card landed before anybody approved it: %+v", landed)
		}

		pending, err := f.db.ListPendingApprovals(ctx, f.project.ID)
		if err != nil || len(pending) != 1 {
			t.Fatalf("%d approvals pending (%v), want 1", len(pending), err)
		}
		if err := f.n.Approve(ctx, pending[0].ID); err != nil {
			t.Fatalf("Approve: %v", err)
		}

		if len(landed) != 1 {
			t.Fatalf("%d landings after approval, want 1", len(landed))
		}
		if landed[0] != (landing{f.project.ID, task.ID, "bbbbbbbbbb"}) {
			t.Errorf("landed %+v, want the project, the card and the commit", landed[0])
		}
	})
}
