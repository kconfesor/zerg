package cerebrate

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

// throttlingAdapter fails fatally with a quota message on its first run and
// succeeds afterwards, which is exactly what a spent window looks like.
type throttlingAdapter struct {
	*scriptedAdapter
	until time.Time

	mu    sync.Mutex
	asked int
}

// Matches on the quota phrase only, exactly as the real adapters do. A double
// that says "throttled" for any text passes against a supervisor that only
// ever hands it the process exit status — which is how the first version of
// this test failed to catch that the non-fatal path was never checked.
func (a *throttlingAdapter) ThrottledBy(text string) (adapter.Throttle, bool) {
	a.mu.Lock()
	a.asked++
	a.mu.Unlock()
	if !strings.Contains(strings.ToLower(text), "usage limit") {
		return adapter.Throttle{}, false
	}
	return adapter.Throttle{Until: a.until, Detail: "hit your ChatGPT usage limit (plus plan)"}, true
}

func (a *throttlingAdapter) timesAsked() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// A spent quota is a pause, not a failure. Failing here is what costs an
// operator the twenty minutes it takes to discover the thing to do was
// nothing — the role resumes by itself.
func TestAQuotaLimitWaitsAndResumesRatherThanFailing(t *testing.T) {
	fatal := &scriptedAdapter{script: `echo 'fatal:You have hit your ChatGPT usage limit'; exit 1`}
	a := &throttlingAdapter{
		scriptedAdapter: fatal,
		// Already past, so the wait is the one-minute grace and the test does
		// not sit on a real quota window.
		until: time.Now().Add(-time.Hour),
	}

	c, _ := newCerebrate(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	waitFor(t, func() bool { return c.State() == StateThrottled }, 5*time.Second,
		"role never entered throttled")

	if got := c.State(); got == StateFailed {
		t.Fatalf("a quota limit failed the role instead of pausing it")
	}
	if c.ThrottledUntil().IsZero() {
		t.Error("throttled with no resume time recorded")
	}
	if a.timesAsked() == 0 {
		t.Error("the adapter was never consulted about the failure")
	}

	// Shutting down while throttled must be clean, not a hang.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while throttled")
	}
	if c.State() != StateStopped {
		t.Errorf("state after shutdown = %q, want stopped", c.State())
	}
}

// claude reports a spent quota window as an ordinary error, not a fatal one,
// so the supervisor has to recognise it on the non-fatal path too. Checking
// only the fatal path is how this first shipped, and it left claude
// crash-looping with exponential backoff through a limit it should wait out.
func TestAQuotaLimitIsCaughtEvenWhenTheHarnessCallsItNonFatal(t *testing.T) {
	// "error:" not "fatal:" — the process exits non-zero with an ordinary
	// error event, exactly as claude does.
	nonFatal := &scriptedAdapter{script: `echo 'error:Claude usage limit reached, resets in 42 minutes'; exit 1`}
	a := &throttlingAdapter{scriptedAdapter: nonFatal, until: time.Now().Add(-time.Hour)}

	c, _ := newCerebrate(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, func() bool { return c.State() == StateThrottled }, 5*time.Second,
		"a non-fatal quota error did not pause the role")
}

// An ordinary fatal error must still fail. If everything paused, a genuinely
// broken role would wait forever and look like it was merely rate limited.
func TestANonQuotaFatalErrorStillFails(t *testing.T) {
	a := &scriptedAdapter{script: `echo 'fatal:invalid api key'; exit 1`}
	c, _ := newCerebrate(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, func() bool { return c.State() == StateFailed }, 5*time.Second,
		"a non-quota fatal error did not fail the role")
}
