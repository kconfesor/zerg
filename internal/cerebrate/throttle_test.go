package cerebrate

import (
	"context"
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

func (a *throttlingAdapter) ThrottledBy(text string) (adapter.Throttle, bool) {
	a.mu.Lock()
	a.asked++
	a.mu.Unlock()
	if text == "" {
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
