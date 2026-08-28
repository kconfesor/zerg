package agent

import (
	"context"
	"net"
	"strings"
	"testing"
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
