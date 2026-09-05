package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

type fixture struct {
	db      *store.DB
	nyd     *nydus.Nydus
	srv     *Server
	project *store.Project
	socket  string
	blobs   *artifact.Store
}

// merges records terminal integration without needing a repository.
type recordingIntegrator struct{ commits, into, published []string }

// Resolve is identity here: these tests pass shas already, and the point of
// the real one is the tree it resolves against, which a fake has none of.
// Publish records the request and returns a plausible URL, so a test can check
// that PR mode published without needing a remote or a GitHub account.
func (r *recordingIntegrator) Publish(_ context.Context, _, base, commit, title, body string, draft bool) (string, error) {
	r.published = append(r.published, title+" -> "+base+"@"+commit[:min(8, len(commit))])
	_ = body
	return "https://example.test/pr/1", nil
}

func (r *recordingIntegrator) Resolve(_ context.Context, _, ref string) (string, error) {
	return ref, nil
}

func (r *recordingIntegrator) MergeInto(_ context.Context, _, commit string) error {
	r.into = append(r.into, commit)
	return nil
}

func (r *recordingIntegrator) Landed(_ context.Context, _, _, commit, _, _ string) (string, bool, error) {
	for _, c := range r.commits {
		if c == commit {
			return commit, true, nil
		}
	}
	return "", false, nil
}

func (r *recordingIntegrator) Merge(_ context.Context, _, _, commit string) error {
	r.commits = append(r.commits, commit)
	return nil
}

func (r *recordingIntegrator) Switch(_ context.Context, _, _, _ string) error { return nil }

func (r *recordingIntegrator) AbortMerge(_ context.Context, _ string) error { return nil }

func (r *recordingIntegrator) Contains(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}

func newFixture(t *testing.T) *fixture {
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
	if err := db.SelectDefaultTeam(ctx, p.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}

	nyd := nydus.New(db, nydus.WithIntegrator(&recordingIntegrator{}))
	blobs := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	srv := NewServer(db, nyd, blobs, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A short socket path: macOS caps unix socket paths near 104 bytes, and a
	// t.TempDir() under /var/folders/... plus a filename can exceed it.
	socket := filepath.Join(t.TempDir(), "z.sock")
	if len(socket) > 100 {
		socket = filepath.Join("/tmp", store.NewID()[:12]+".sock")
		t.Cleanup(func() { _ = exec.Command("rm", "-f", socket).Run() })
	}
	if err := srv.Listen(socket); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	return &fixture{db: db, nyd: nyd, srv: srv, project: p, socket: socket, blobs: blobs}
}

func (f *fixture) client(t *testing.T, role string) *Client {
	t.Helper()
	return NewClient(f.socket, f.srv.Mint(f.project.ID, role))
}

// The whole point: an agent claims, works, hands on, and acknowledges — with no
// keystrokes, no scripts on its PATH, and no directory to infer identity from.
func TestAgentClaimsWorksAndHandsOn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "build a calculator", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	coder := f.client(t, "coder")
	work, err := coder.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if len(work.Items) != 1 {
		t.Fatalf("claimed %d items, want 1", len(work.Items))
	}
	if work.Items[0].TaskName != "Calculator" {
		t.Errorf("task name = %q; the name follows the card through the pipeline", work.Items[0].TaskName)
	}
	if work.Task == nil || work.Task.ID != task.ID {
		t.Error("the claimed work did not carry its task")
	}
	if work.LeaseID == "" || work.ExpiresAt.IsZero() {
		t.Error("work must arrive with a lease and a deadline")
	}

	if _, err := coder.Send(ctx, SendArgs{
		To: "reviewer", TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "implemented",
	}); err != nil {
		t.Fatalf("coder Send: %v", err)
	}
	if err := coder.Done(ctx, work.LeaseID); err != nil {
		t.Fatalf("coder Done: %v", err)
	}

	reviewer := f.client(t, "reviewer")
	got, err := reviewer.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("reviewer Next: %v", err)
	}
	if got.Items[0].From != "coder" {
		t.Errorf("work came from %q, want coder", got.Items[0].From)
	}
	// This fixture has no worktree, so no merge can have happened, and the
	// envelope has to say so. The assertion here used to be the opposite —
	// "a handoff carrying a commit must say it was already merged" — which
	// passed because the field was derived from the same commit the test had
	// just attached. It restated the implementation instead of checking an
	// outcome, so it stayed green through a release where nothing merged
	// anything at all. TestClaimMergesHandoffIntoWorktree owns the real
	// property, against a repository where a merge can actually occur.
	if got.Items[0].Merged {
		t.Error("merged must report the merge, not the presence of a commit")
	}
}

// No work is an ordinary outcome, not a failure. An agent told there is nothing
// should stop rather than invent a retry loop.
func TestNextReturnsErrNoWorkWhenIdle(t *testing.T) {
	f := newFixture(t)
	_, err := f.client(t, "coder").Next(context.Background(), 0)
	if !errors.Is(err, ErrNoWork) {
		t.Fatalf("got %v, want ErrNoWork", err)
	}
}

// Waiting is how an agent avoids polling. The lease model makes it safe: a
// missed poll costs nothing, unlike a wake-up with one chance to be noticed.
func TestNextWaitsForWorkToArrive(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	go func() {
		time.Sleep(300 * time.Millisecond)
		if _, err := f.nyd.NewTask(ctx, f.project.ID, "Later", "arrives after the wait began", "", nil); err != nil {
			t.Errorf("NewTask: %v", err)
		}
	}()

	start := time.Now()
	work, err := f.client(t, "coder").Next(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("Next returned before the work existed")
	}
	if len(work.Items) != 1 {
		t.Errorf("claimed %d items, want 1", len(work.Items))
	}
}

// A token proves one role. A sender read from an environment variable is a
// sender any agent can set to any value.
func TestTokenScopesTheSender(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "x", "", nil); err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// reviewer's token cannot claim coder's queue.
	if _, err := f.client(t, "reviewer").Next(ctx, 0); !errors.Is(err, ErrNoWork) {
		t.Errorf("reviewer saw coder's work: %v", err)
	}

	// And a role cannot send as another: coder is not terminal, so an
	// omitted recipient must be refused rather than treated as completion.
	if _, err := f.client(t, "coder").Send(ctx, SendArgs{Commit: "aaaaaaaaaa", Body: "handed on"}); err == nil {
		t.Error("a mid-pipeline role was allowed to finish a task")
	}
}

func TestUnknownTokenIsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := NewClient(f.socket, "not-a-real-token").Next(context.Background(), 0)
	if err == nil {
		t.Fatal("an unrecognised token was served")
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	f := newFixture(t)
	token := f.srv.Mint(f.project.ID, "coder")
	c := NewClient(f.socket, token)

	if _, err := c.Next(context.Background(), 0); !errors.Is(err, ErrNoWork) {
		t.Fatalf("a fresh token should work: %v", err)
	}
	f.srv.Revoke(token)
	if _, err := c.Next(context.Background(), 0); err == nil || errors.Is(err, ErrNoWork) {
		t.Error("a revoked token still worked")
	}
}

// Acking twice must not be an error: an agent that crashed after acking and
// retried on restart did nothing wrong.
func TestDoneIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "x", "", nil); err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	c := f.client(t, "coder")
	work, err := c.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := c.Done(ctx, work.LeaseID); err != nil {
		t.Fatalf("first Done: %v", err)
	}
	if err := c.Done(ctx, work.LeaseID); err != nil {
		t.Errorf("second Done: %v", err)
	}
}

func TestDoneRejectsAnUnknownLease(t *testing.T) {
	f := newFixture(t)
	if err := f.client(t, "coder").Done(context.Background(), "NOSUCHLEASE"); err == nil {
		t.Fatal("acknowledging a lease that does not exist was accepted")
	}
}

// A question with no answer must not look like an agent that stopped for no
// reason.
func TestAskRecordsAQuestionAndReceivesAnAnswer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	go func() {
		// Stand in for an operator answering in Attention.
		for i := 0; i < 40; i++ {
			open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
			if err == nil && len(open) > 0 {
				_ = f.db.AnswerClarification(ctx, store.DecisionScope{}, open[0].ID, "yes, make it idempotent", "")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	answer, err := f.client(t, "coder").Ask(ctx, "should this be idempotent?", "", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !answer.Answered || answer.Answer != "yes, make it idempotent" {
		t.Errorf("got %+v, want the operator's answer", answer)
	}
}

// The incident this fixes, end to end.
//
// The ask waits, gives up, and reports the question as pending. The agent asks
// again, and both halves used to go wrong at once: a second identical card in
// the operator's panel, and the answer typed into the first one written to a
// row nobody was waiting on. Two options were chosen six seconds apart on one
// real card and the agent acted on the second, having never seen the first.
func TestAnAnswerGivenAfterTheWaitRanOutReachesTheNextAsk(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	coder := f.client(t, "coder")

	offered := []string{"Redis, shared across instances", "A signed cookie"}
	const question = "Where does the session live?"

	// Waits for nothing, so it comes back pending, exactly as it did at ten
	// minutes on the card this is about.
	pending, err := coder.Ask(ctx, question, "", offered, 0)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if pending.Answered {
		t.Fatal("a question nobody had answered came back answered")
	}

	// The operator answers now, with the agent no longer listening.
	if err := f.db.AnswerClarification(ctx, store.DecisionScope{}, pending.ID, offered[1], ""); err != nil {
		t.Fatalf("AnswerClarification: %v", err)
	}

	again, err := coder.Ask(ctx, question, "", offered, 0)
	if err != nil {
		t.Fatalf("asking again: %v", err)
	}
	if again.ID != pending.ID {
		t.Errorf("the repeat filed a second question (%s, then %s)", pending.ID, again.ID)
	}
	if !again.Answered || again.Answer != offered[1] {
		t.Errorf("got %+v, want the answer the operator had already given", again)
	}

	// And the operator was never asked twice for one decision.
	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d question(s) still open after the answer was collected", len(open))
	}
}

// An agent that already knows the answers offers them, and gets back the text
// of the one that was chosen. Matching the answer against what was offered is
// the whole point: before this the operator retyped it, and "the API" came
// back as "the api", or a paraphrase, or a typo.
func TestAskOffersOptionsAndReturnsTheChosenOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	offered := []string{"admin", "customer", "the API"}
	go func() {
		for i := 0; i < 40; i++ {
			open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
			if err == nil && len(open) > 0 {
				if !slices.Equal(open[0].Options, offered) {
					t.Errorf("the operator was offered %q, want %q", open[0].Options, offered)
				}
				// What the cockpit posts when the third radio is picked.
				_ = f.db.AnswerClarification(ctx, store.DecisionScope{}, open[0].ID, open[0].Options[2], "")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	answer, err := f.client(t, "coder").Ask(ctx, "which of these should I serve?", "", offered, 5*time.Second)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !answer.Answered || answer.Answer != "the API" {
		t.Errorf("got %+v, want the option that was chosen, verbatim", answer)
	}
}

// Options an operator cannot choose between are the agent's mistake, and it is
// told so rather than having the question filed anyway.
func TestAskRefusesOptionsThatCannotBeChosenBetween(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.client(t, "coder").Ask(ctx, "which?", "", []string{"Redis", "Redis"}, 0); err == nil {
		t.Error("a question offering the same option twice was accepted")
	}
	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("%d refused question(s) reached the operator anyway", len(open))
	}
}

// An unanswered question is reported as pending rather than hanging, so the
// agent can decide what to do about it.
func TestAskReturnsPendingRatherThanHanging(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	answer, err := f.client(t, "coder").Ask(ctx, "is anyone there?", "", nil, 0)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer.Answered {
		t.Error("an unanswered question reported an answer")
	}
	if answer.ID == "" {
		t.Error("a pending question must come back with an id")
	}

	open, err := f.db.ListOpenClarifications(ctx, f.project.ID)
	if err != nil {
		t.Fatalf("ListOpenClarifications: %v", err)
	}
	if len(open) != 1 || open[0].Role != "coder" {
		t.Errorf("the question was not recorded for the operator: %+v", open)
	}
}

// The terminal role finishes rather than forwarding, and integration is the
// orchestrator's to perform.
func TestTerminalRoleCompletesTheTask(t *testing.T) {
	ctx := context.Background()
	integrator := &recordingIntegrator{}
	f := newFixture(t)
	f.nyd = nydus.New(f.db, nydus.WithIntegrator(integrator))
	f.srv = NewServer(f.db, f.nyd, f.blobs, slog.New(slog.NewTextHandler(io.Discard, nil)))

	socket := filepath.Join("/tmp", store.NewID()[:12]+".sock")
	if err := f.srv.Listen(socket); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { f.srv.Close() })
	f.socket = socket

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "x", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	coder := f.client(t, "coder")
	work, err := coder.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if _, err := coder.Send(ctx, SendArgs{To: "reviewer", TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "handed on"}); err != nil {
		t.Fatalf("coder Send: %v", err)
	}
	if err := coder.Done(ctx, work.LeaseID); err != nil {
		t.Fatalf("coder Done: %v", err)
	}

	reviewer := f.client(t, "reviewer")
	rwork, err := reviewer.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("reviewer Next: %v", err)
	}
	// No recipient: the terminal role finishing the task.
	if _, err := reviewer.Send(ctx, SendArgs{TaskID: task.ID, Commit: "cccccccccc", Body: "handed on"}); err != nil {
		t.Fatalf("reviewer completion: %v", err)
	}
	if err := reviewer.Done(ctx, rwork.LeaseID); err != nil {
		t.Fatalf("reviewer Done: %v", err)
	}

	done, err := f.db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if done.Lane != store.LaneDone || done.State != store.TaskDone {
		t.Errorf("card is %s/%s, want done/done", done.Lane, done.State)
	}
	if len(integrator.commits) != 1 || integrator.commits[0] != "cccccccccc" {
		t.Errorf("integrated %v, want the terminal commit", integrator.commits)
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

// A token scopes an agent to one project, and a task id must be scoped the
// same way. The lookup used to be global: any valid id was accepted, so an
// agent in project A could hand off against project B's card, and the id then
// travelled into routing and task updates unchecked.
func TestAnAgentCannotNameAnotherProjectsTask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A second project with a card of its own.
	other, err := f.db.CreateProject(ctx, mustDir(t, "other"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := f.db.SelectDefaultTeam(ctx, other.ID); err != nil {
		t.Fatalf("SelectDefaultTeam: %v", err)
	}
	foreign, err := f.db.CreateTask(ctx, other.ID, "Theirs", "not yours", "coder")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	c := f.client(t, "coder")

	// The id is real, and belongs to someone else.
	if _, err := c.Send(ctx, SendArgs{To: "reviewer", Kind: "note", TaskID: foreign.ID, Body: "x"}); err == nil {
		t.Error("send accepted a task id from another project")
	}
	if _, err := c.Ask(ctx, "which one?", foreign.ID, nil, 0); err == nil {
		t.Error("ask accepted a task id from another project")
	}

	// The agent's own project still works by id and by name, once it is
	// actually holding the card.
	mine, err := f.nyd.NewTask(ctx, f.project.ID, "Mine", "yours", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if _, err := c.Next(ctx, 2*time.Second); err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if _, err := c.Send(ctx, SendArgs{To: "reviewer", Kind: "note", TaskID: mine.ID, Body: "by id"}); err != nil {
		t.Errorf("send rejected this project's task by id: %v", err)
	}
	if _, err := c.Send(ctx, SendArgs{To: "reviewer", Kind: "note", TaskID: "Mine", Body: "by name"}); err != nil {
		t.Errorf("send rejected this project's task by name: %v", err)
	}
}

// A token proves a role, not a claim. Membership of the project used to be the
// whole check, so a terminal role could finish a card it had never been handed
// — including one another role was working on at that moment.
func TestSendRequiresHoldingTheTask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "build a calculator", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// reviewer is on the team and this card has never reached it.
	reviewer := f.client(t, "reviewer")
	if _, err := reviewer.Send(ctx, SendArgs{
		TaskID: task.ID, Commit: "abc123", Body: "shipped it"}); err == nil {
		t.Error("the terminal role completed a task it had never claimed")
	}

	// The role that does hold it may act on it.
	coder := f.client(t, "coder")
	if _, err := coder.Next(ctx, 2*time.Second); err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if _, err := coder.Send(ctx, SendArgs{
		To: "reviewer", TaskID: task.ID, Commit: "abc123", Body: "have a look"}); err != nil {
		t.Errorf("the holder was refused: %v", err)
	}
}

// A lost response is retried, and the retry must not produce a second hand-off:
// two routes, two claims, and two turns of model work on one result.
func TestRepeatedSendIsAbsorbed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Calculator", "build a calculator", "", nil)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	coder := f.client(t, "coder")
	if _, err := coder.Next(ctx, 2*time.Second); err != nil {
		t.Fatalf("coder Next: %v", err)
	}

	args := SendArgs{To: "reviewer", TaskID: task.ID, Commit: "abc123", Body: "have a look"}
	if _, err := coder.Send(ctx, args); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := coder.Send(ctx, args); err != nil {
		t.Fatalf("retried send: %v", err)
	}

	n, err := f.db.QueuedCount(ctx, f.project.ID, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reviewer has %d queued items after one send retried; want 1", n)
	}
}

// ── a card that skips a role ──────────────────────────────────────────────

// roleID is the library id of a role by name, which is what a skip list holds.
func (f *fixture) roleID(t *testing.T, name string) string {
	t.Helper()
	tpl, err := f.db.GetTemplateByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetTemplateByName(%q): %v", name, err)
	}
	return tpl.ID
}

// The envelope is the only thing that tells an agent where its work goes, so a
// card that skips a role has to be answered there. Skipping the role that
// merges makes the one before it terminal, and it has to be told so: an agent
// that reads "terminal": false hands the work to a role this card does not
// visit, and the card stops.
func TestTheEnvelopeRoutesRoundASkippedRole(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if _, err := f.nyd.NewTask(ctx, f.project.ID, "Fix a typo", "one line", "",
		[]string{f.roleID(t, "reviewer")}); err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	work, err := f.client(t, "coder").Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if !work.Terminal {
		t.Error("terminal = false; the reviewer is skipped, so the coder finishes this card")
	}
	if work.Next != "" {
		t.Errorf("next = %q, want nothing — there is no role after the coder on this card", work.Next)
	}
}

// A skipped role that is sent to anyway still gets an answer. That is what
// rework is: a reviewer that finds a problem on a card whose coder was skipped
// has to be able to send the work back, and the coder then has to know where
// to return it.
func TestASkippedRoleThatIsSentToIsStillToldWhereWorkGoes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Docs", "just words", "",
		[]string{f.roleID(t, "coder")})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// The card opened on the reviewer, since the coder is not on its route.
	reviewer := f.client(t, "reviewer")
	work, err := reviewer.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("reviewer Next: %v", err)
	}
	if !work.Terminal {
		t.Error("terminal = false on the only role this card visits")
	}

	// It sends the work back to the coder anyway, which is allowed.
	if _, err := reviewer.Send(ctx, SendArgs{
		To: "coder", TaskID: task.ID, Commit: "aaaaaaaaaa", Body: "needs a change"}); err != nil {
		t.Fatalf("reviewer Send to a skipped role: %v", err)
	}
	if err := reviewer.Done(ctx, work.LeaseID); err != nil {
		t.Fatalf("reviewer Done: %v", err)
	}

	back, err := f.client(t, "coder").Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("coder Next: %v", err)
	}
	if back.Next != "reviewer" {
		t.Errorf("next = %q, want reviewer — a skipped role rejoins the route after itself", back.Next)
	}
	if back.Terminal {
		t.Error("terminal = true on a role the card skipped; the reviewer is still ahead of it")
	}
}

// A card that cannot be read must not be routed as though it skipped nothing.
//
// describe used to drop every error from reading the task and carry on with an
// empty skip list, which answers `next` and `terminal` from the full team: an
// agent told to hand work to a role the card skips, or that it may finish a
// card it may not. Neither is visible from the agent's side -- the envelope
// looks ordinary -- so the failure is silent, and this is what keeps it gone.
func TestAnUnreadableCardIsAnErrorRatherThanAFullTeamRoute(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	task, err := f.nyd.NewTask(ctx, f.project.ID, "Fix a typo", "one line", "",
		[]string{f.roleID(t, "reviewer")})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}

	// The column, as a database error or a bad hand edit would leave it. The
	// claim itself still works: nydus compares the skip list as an opaque
	// string when it batches, so the fault surfaces where it is parsed.
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE tasks SET skip = ? WHERE id = ?`, "{not json", task.ID); err != nil {
		t.Fatalf("corrupting the skip list: %v", err)
	}

	work, err := f.client(t, "coder").Next(ctx, 2*time.Second)
	if err == nil {
		t.Fatalf("claimed work with next=%q terminal=%v on a card that could not be read;"+
			" an unreadable route must not fall back to the whole team", work.Next, work.Terminal)
	}
	if errors.Is(err, ErrNoWork) {
		t.Fatal("reported no work; the work exists and the card behind it is unreadable")
	}
	if !strings.Contains(err.Error(), task.ID) {
		t.Errorf("err = %v, want it to name the card that could not be read", err)
	}

	// Repaired, the same claim describes itself normally — so the failure above
	// was the corrupt column and nothing else about this fixture.
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE tasks SET skip = ? WHERE id = ?`, "", task.ID); err != nil {
		t.Fatalf("repairing the skip list: %v", err)
	}
	work, err = f.client(t, "coder").Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("Next after repair: %v", err)
	}
	if work.Next != "reviewer" {
		t.Errorf("next = %q after repair, want reviewer", work.Next)
	}
}
