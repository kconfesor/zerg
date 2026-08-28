package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Listing models runs the harness's own CLI: 1.9 seconds for pi on the machine
// this was measured on, and the cockpit asks for every harness on every load.
func TestAHarnessIsAskedForItsModelsOnce(t *testing.T) {
	ctx := context.Background()
	c := newCatalog()

	var calls int
	list := func(context.Context) ([]adapter.Model, error) {
		calls++
		return []adapter.Model{{ID: "sonnet"}}, nil
	}

	for i := 0; i < 3; i++ {
		got, err := c.models(ctx, "pi", list)
		if err != nil || len(got) != 1 || got[0].ID != "sonnet" {
			t.Fatalf("models = %+v, %v", got, err)
		}
	}
	if calls != 1 {
		t.Errorf("the harness was asked %d times, want once", calls)
	}

	// A second harness is a second catalogue, not a shared one.
	if _, err := c.models(ctx, "claude", list); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("%d calls after a second harness, want 2", calls)
	}
}

// A page asking for four harnesses at once must not start four processes, and
// must not leave the cache empty because none of them finished in time.
func TestConcurrentCallersShareOneListing(t *testing.T) {
	ctx := context.Background()
	c := newCatalog()

	var mu sync.Mutex
	calls := 0
	list := func(context.Context) ([]adapter.Model, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // a CLI, slowly
		return []adapter.Model{{ID: "one"}}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.models(ctx, "pi", list); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("%d listings for eight callers, want one", calls)
	}
}

// A harness that is not installed yet may be installed a moment later.
// Remembering the failure would keep the cockpit wrong for ten minutes after
// the operator fixed the thing it complained about.
func TestAFailureIsNotRemembered(t *testing.T) {
	ctx := context.Background()
	c := newCatalog()

	var calls int
	failing := func(context.Context) ([]adapter.Model, error) {
		calls++
		return nil, errors.New("pi is not on PATH")
	}
	if _, err := c.models(ctx, "pi", failing); err == nil {
		t.Fatal("the error did not reach the caller")
	}
	if _, err := c.models(ctx, "pi", failing); err == nil {
		t.Fatal("the error did not reach the caller the second time")
	}
	if calls != 2 {
		t.Errorf("%d attempts, want the second to try again", calls)
	}

	// And once it works, that answer is the one kept.
	if _, err := c.models(ctx, "pi", func(context.Context) ([]adapter.Model, error) {
		return []adapter.Model{{ID: "sonnet"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := c.models(ctx, "pi", failing)
	if err != nil || len(got) != 1 {
		t.Errorf("after a success the cache holds %+v (%v)", got, err)
	}
}
