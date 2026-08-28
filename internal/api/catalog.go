package api

import (
	"context"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Remembering what a harness said its models were.
//
// Listing them means running the harness's own CLI: `pi --list-models`
// measured 1.9 seconds on the machine this was written on, and the cockpit
// asks for every harness on every load, then again whenever Settings or a role
// dialog opens. Nothing about the answer changes between those calls -- a
// model catalogue changes when the harness is upgraded, not while a page is
// being read.
//
// A time-to-live rather than forever, because a harness upgraded under a
// running daemon should be picked up without restarting it; ten minutes is
// short enough that nobody hunts for a stale list and long enough that a
// session of clicking around costs one process.
//
// The per-key lock is what makes it worth having. Without it, a page that asks
// for four harnesses at once on a cold cache runs four CLIs at once, and the
// second load does it again because none of them had finished in time to be
// stored. Held across the call, the first caller pays and the rest wait for
// the same answer.
const catalogTTL = 10 * time.Minute

type catalog struct {
	mu   sync.Mutex
	byID map[string]*catalogEntry
}

type catalogEntry struct {
	mu     sync.Mutex
	at     time.Time
	models []adapter.Model
	err    error
}

func newCatalog() *catalog {
	return &catalog{byID: map[string]*catalogEntry{}}
}

// models returns the harness's catalogue, from memory when it is fresh.
//
// An error is not cached: a harness that was not installed a minute ago may be
// installed now, and holding on to the failure would make the cockpit wrong
// for ten minutes after the operator fixed it.
func (c *catalog) models(ctx context.Context, name string, list func(context.Context) ([]adapter.Model, error)) ([]adapter.Model, error) {
	c.mu.Lock()
	e := c.byID[name]
	if e == nil {
		e = &catalogEntry{}
		c.byID[name] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil && e.models != nil && time.Since(e.at) < catalogTTL {
		return e.models, nil
	}
	models, err := list(ctx)
	if err != nil {
		return nil, err
	}
	e.models, e.err, e.at = models, nil, time.Now()
	return models, nil
}
