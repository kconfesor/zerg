// Package event carries typed events from agents to whoever is watching.
//
// The alternative answer to "what is this agent doing" is grepping a terminal
// pane for a line containing "I'm". Everything here exists so that question has
// a real answer instead: structured, filterable, and replayable after a reload.
package event

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Event is an adapter event stamped with where it came from.
type Event struct {
	adapter.Event

	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`
}

// Bus fans events out to subscribers.
//
// Subscribers are non-blocking by construction: a browser that stops reading
// must never stall the agent producing the events. A slow subscriber drops
// events and counts them, so the gap is visible rather than silent.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]*subscription
	nextID int
}

type subscription struct {
	ch chan Event

	// dropped is atomic because Publish counts it while holding only a read
	// lock. A read lock lets publishers run concurrently, so an ordinary
	// increment here is a data race — the counter that records dropped events
	// would itself be the bug.
	dropped atomic.Int64
}

func NewBus() *Bus { return &Bus{subs: map[int]*subscription{}} }

// Subscribe returns a channel of events and a function that closes it.
//
// buffer sizes the subscriber's queue. Once it is full, further events are
// dropped for that subscriber alone.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	sub := &subscription{ch: make(chan Event, buffer)}
	b.subs[id] = sub
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// Publish delivers to every current subscriber without blocking on any of them.
func (b *Bus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			// Dropping is deliberate. Blocking here would let a stalled reader
			// halt the agent it is watching, which trades an observability
			// problem for a liveness one.
			sub.dropped.Add(1)
		}
	}
}

// Subscribers reports how many are attached, for health reporting.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Dropped totals the events current subscribers never received.
//
// A gap in an event feed should be a number someone can see, not something the
// reader infers from a story that stops making sense.
func (b *Bus) Dropped() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var total int64
	for _, sub := range b.subs {
		total += sub.dropped.Load()
	}
	return total
}
