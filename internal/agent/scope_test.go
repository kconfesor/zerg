package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

// listening is something for a service registration to point at.
func listening(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func port(ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

// A token that carries three verbs cannot call the fourth.
//
// The runner is the reason this exists: it registers what it started, asks
// when it is stuck, and writes down what it learned, and it must never be able
// to put work into the queue -- especially once it starts on its own after a
// task lands. A capability that is not needed and not granted cannot be
// misused by an agent that read the wrong file.
func TestAScopedTokenCanOnlyDoItsJob(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")

	scoped := NewClient(f.socket,
		f.srv.MintScoped(f.project.ID, "runner", CanArtifact, CanAsk, CanRemember))

	// What it is for: registering a service it started.
	ln := listening(t)
	defer ln.Close()
	if _, err := scoped.Artifact(ctx, ArtifactArgs{
		Kind: "service", Port: port(ln), Label: "preview",
	}); err != nil {
		t.Errorf("a runner could not register the service it started: %v", err)
	}
	// And writing down what it learned.
	if err := scoped.Remember(ctx, "serves with: ./serve.sh"); err != nil {
		t.Errorf("a runner could not remember: %v", err)
	}

	// What it is not for. The refusal names the verb, because the agent
	// reading it is deciding what to do next.
	if _, err := scoped.Next(ctx, 0); err == nil {
		t.Error("a runner claimed work")
	} else if !strings.Contains(err.Error(), "cannot next") {
		t.Errorf("refusal was %q, which does not say what was refused", err)
	}
	if _, err := scoped.Send(ctx, SendArgs{To: "coder", Body: "do this"}); err == nil {
		t.Error("a runner sent work into the pipeline")
	}
	if err := scoped.Done(ctx, "some-lease"); err == nil {
		t.Error("a runner acknowledged a lease")
	}
}

// A pipeline role's token is unscoped and keeps every verb, which is what
// every existing test and every real role depends on.
func TestAnUnscopedTokenKeepsEveryVerb(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")

	c := f.client(t, "coder")
	if _, err := c.Next(ctx, 0); err != nil {
		t.Errorf("a role could not claim work: %v", err)
	}
}

// Split is never implied. A pipeline token that could spawn ten subtasks
// would spend the money the operator gate exists to stop.
func TestSplitIsNotImpliedOnAPipelineToken(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "")
	if err != nil {
		t.Fatal(err)
	}
	c := f.client(t, "coder")
	if _, err := c.Split(ctx, feat.Name, "", []store.PlanDraft{{Name: "Schema"}}); err == nil {
		t.Error("a pipeline role submitted a plan")
	} else if !strings.Contains(err.Error(), "cannot split") {
		t.Errorf("refusal was %q, which does not say what was refused", err)
	}
}

func TestSupervisorNextReturnsAPlan(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	feat, err := f.db.CreateFeature(ctx, f.project.ID, "Billing", "rewrite invoicing")
	if err != nil {
		t.Fatal(err)
	}
	sup := NewClient(f.socket, f.srv.MintScoped(f.project.ID, "supervisor",
		CanClaim, CanAsk, CanDecide, CanSplit))
	work, err := sup.Next(ctx, 0)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if work.Kind != "plan" || work.Task == nil || work.Task.ID != feat.ID {
		t.Fatalf("kind=%q task=%v, want a plan for the feature", work.Kind, work.Task)
	}
	rev, err := sup.Split(ctx, feat.Name, "deadbeef", []store.PlanDraft{
		{Name: "Schema", Body: "the tables"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev.State != store.PlanPending || rev.ItemCount != 1 {
		t.Fatalf("revision = %+v", rev)
	}
	if _, err := sup.Next(ctx, 0); !errors.Is(err, ErrNoWork) {
		t.Errorf("next after submitting = %v, want no work while the operator decides", err)
	}
}

// Whose process is it, which decides what stops it.
//
// A pipeline role's dev server dies when the swarm stops, because the agent
// that started it is part of the swarm. A runner is not: the daemon spawned
// it, it outlives Start and Stop, and marking its preview dead when the
// pipeline goes down would take away the thing somebody is looking at.
func TestWhatARunnerStartsSurvivesTheSwarmStopping(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	roleLn, runnerLn := listening(t), listening(t)
	defer roleLn.Close()
	defer runnerLn.Close()

	// A pipeline role registers one, the ordinary way.
	role := f.client(t, "coder")
	if _, err := role.Next(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := role.Artifact(ctx, ArtifactArgs{
		Kind: "service", Port: port(roleLn), Label: "the coder's dev server",
	}); err != nil {
		t.Fatal(err)
	}

	// And a runner, spawned by the daemon for that card.
	runner := NewClient(f.socket, f.srv.MintFor(f.project.ID, "runner", task.ID,
		CanArtifact, CanAsk, CanRemember))
	preview, err := runner.Artifact(ctx, ArtifactArgs{
		Kind: "service", Port: port(runnerLn), Label: "the preview",
	})
	if err != nil {
		t.Fatal(err)
	}
	// It attaches to the card it was spawned for, having no lease to find one
	// through.
	if preview.TaskID == nil || *preview.TaskID != task.ID {
		t.Errorf("the preview landed on %v, want the card that asked for it", preview.TaskID)
	}

	// The swarm goes down, which is what Stop does.
	if n, err := f.db.StopServices(ctx, f.project.ID, store.OwnerAgent); err != nil || n != 1 {
		t.Fatalf("stopping the swarm's services stopped %d (%v), want the one", n, err)
	}
	after, err := f.db.GetArtifact(ctx, preview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Live() {
		t.Error("the preview was marked dead when the pipeline stopped, and it is still running")
	}
}
