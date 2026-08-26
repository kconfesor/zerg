package event

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/store"
)

// recorderBuffer is the bus subscription's depth. Kept generous, but it is no
// longer what protects the records: the reader below drains it into a much
// larger queue immediately, so the channel is empty again within microseconds
// of a burst arriving.
const recorderBuffer = 4096

// queueWarn is the depth at which a backlog stops being a burst and starts
// being a problem worth saying out loud.
const queueWarn = 10_000

// queueMax bounds the queue, and shedding is explicit rather than accidental.
//
// Unbounded was the wrong end of the trade. A writer that cannot keep up — a
// locked database, a disk that has stopped answering — grew the queue for the
// lifetime of the daemon, so an observability problem became an out-of-memory
// one and took the run with it. At roughly 300 bytes an event this is ~30 MB,
// far past any burst a real run produces.
//
// What gets dropped is deliberate: display events first, usage last. A missing
// transcript line is a gap in a story; a missing usage row is money spent that
// nothing anywhere records. Every drop is counted and reported by health.
const queueMax = 100_000

// Recorder persists the event stream: every event for replay, and usage
// separately for cost.
//
// Both come off one subscription rather than two, so a usage event resolves
// which card it belongs to once instead of twice, and the two records can never
// disagree about the order they arrived in.
//
// What it must not do is slow an agent down — blocking the bus would let a
// stalled writer halt the run it is watching, trading an observability problem
// for a liveness one. But it also must not share the browser's semantics. The
// bus drops when a subscriber's buffer fills, and this subscriber writes the
// usage rows the cost accounting is made of: a burst, or one slow SQLite
// commit, silently cost real money from the record.
//
// So the channel is drained into a large bounded queue by a reader that does
// nothing else, and the database writes happen behind it. The bus never waits
// on a disk write, and a backlog costs memory rather than rows. The bound is
// what keeps that trade honest: past it the queue sheds display events before
// usage rows, and says how many, instead of growing until the process dies.
type Recorder struct {
	mu     sync.Mutex
	queue  []Event
	wake   chan struct{}
	closed bool

	queued  atomic.Int64 // current depth, for health
	dropped atomic.Int64 // events the bus never handed over
	written atomic.Int64
	failed  atomic.Int64
	peak    atomic.Int64
}

// Stats is what the health endpoint reports. A gap in the record should be a
// number someone can see rather than something inferred from a story that stops
// making sense.
type Stats struct {
	Queued  int64 `json:"queued"`
	Peak    int64 `json:"peakQueued"`
	Dropped int64 `json:"dropped"`
	Written int64 `json:"written"`
	Failed  int64 `json:"failed"`
}

func (r *Recorder) Stats() Stats {
	return Stats{
		Queued:  r.queued.Load(),
		Peak:    r.peak.Load(),
		Dropped: r.dropped.Load(),
		Written: r.written.Load(),
		Failed:  r.failed.Load(),
	}
}

func (r *Recorder) push(ev Event) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	shed := 0
	if len(r.queue) >= queueMax {
		shed = r.shedLocked()
	}
	r.queue = append(r.queue, ev)
	depth := int64(len(r.queue))
	r.mu.Unlock()

	if shed > 0 {
		r.dropped.Add(int64(shed))
	}
	r.queued.Store(depth)
	if depth > r.peak.Load() {
		r.peak.Store(depth)
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// shedLocked makes room for one event and reports how many it discarded.
//
// The oldest display event goes first, because the newest are what someone
// watching is reading and the oldest are the ones already scrolled past. A
// usage event is only discarded when the queue holds nothing else — at which
// point the run has produced 100,000 unwritten turns and the record is beyond
// saving either way.
func (r *Recorder) shedLocked() int {
	for i, ev := range r.queue {
		if ev.Kind != adapter.EventUsage {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return 1
		}
	}
	r.queue = r.queue[1:]
	return 1
}

// take removes up to n events. It returns nil when the queue is empty.
func (r *Recorder) take(n int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return nil
	}
	if n > len(r.queue) {
		n = len(r.queue)
	}
	batch := r.queue[:n:n]
	r.queue = r.queue[n:]
	r.queued.Store(int64(len(r.queue)))
	return batch
}

// Record starts the recorder and returns it, so health can read its counters.
func Record(ctx context.Context, bus *Bus, db *store.DB, log *slog.Logger) *Recorder {
	r := &Recorder{wake: make(chan struct{}, 1)}
	ch, cancel := bus.Subscribe(recorderBuffer)

	// The reader does nothing but move events off the bus, so the only way to
	// lose one is for the whole process to fall behind by 4096 events while
	// doing no database work at all.
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.closed = true
			r.mu.Unlock()
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}()
		before := bus.Dropped()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.ProjectID == "" {
					continue
				}
				r.push(ev)
				if now := bus.Dropped(); now > before {
					r.dropped.Add(now - before)
					log.Warn("event bus dropped records before the recorder saw them",
						"dropped", now-before)
					before = now
				}
			}
		}
	}()

	// The writer. Separate goroutine so a slow commit delays persistence and
	// nothing else.
	go func() {
		for {
			batch := r.take(256)
			if batch == nil {
				r.mu.Lock()
				done := r.closed
				r.mu.Unlock()
				if done {
					return
				}
				select {
				case <-ctx.Done():
					// Drain what is already queued: these are rows an agent
					// has already produced, and the run is over, so nothing is
					// waiting on this.
					for rest := r.take(256); rest != nil; rest = r.take(256) {
						r.write(context.WithoutCancel(ctx), db, log, rest)
					}
					return
				case <-r.wake:
				}
				continue
			}
			if depth := r.queued.Load(); depth > queueWarn {
				log.Warn("event recorder is falling behind", "queued", depth)
			}
			r.write(ctx, db, log, batch)
		}
	}()

	return r
}

// write persists one batch.
func (r *Recorder) write(ctx context.Context, db *store.DB, log *slog.Logger, batch []Event) {
	for _, ev := range batch {
		// Which card the work belonged to, through the lease the role held
		// when the event was produced — not the one it holds now. The queue
		// means those are different questions: an event emitted under task A
		// can reach this line after the role has claimed task B, and asking
		// for the newest lease attributes A's tokens and A's transcript to B.
		// The event's timestamp does not move, so neither does the answer.
		//
		// A miss is ordinary — agents emit events before claiming anything —
		// and never a reason to drop the row.
		//
		// This is a three-table join per event. At observed volumes (~20
		// events per turn) that is nothing against a local WAL database; if it
		// ever shows up, the fix is a short-TTL cache keyed by role and lease,
		// not dropping the attribution.
		taskID, err := db.TaskForAt(ctx, ev.ProjectID, ev.Role, ev.At)
		if err != nil {
			log.Debug("event: could not attribute to a task",
				"role", ev.Role, "err", err)
		}

		e := &store.Event{
			// The id was assigned at publish and is a monotonic ULID, so the
			// value the browser holds as Last-Event-ID is the same value stored
			// here. Reusing it is what makes resume-after-reconnect exact.
			ID:        ev.ID,
			ProjectID: ev.ProjectID,
			TaskID:    taskID,
			Role:      ev.Role,
			Kind:      string(ev.Kind),
			At:        ev.At,
			Text:      ev.Text,
			Tool:      ev.Tool,
			Fatal:     ev.Fatal,
			Data:      Payload(ev),
		}

		// The event and the usage it carried go in together. Written
		// separately, a failure between them left the transcript and the ledger
		// describing the same turn differently — a turn with no cost, or a cost
		// with no turn — and a harness reports a turn once, so neither is
		// recoverable afterwards.
		var turn *store.UsageTurn
		if ev.Kind == adapter.EventUsage {
			u := usageOf(ev, taskID)
			turn = &u
		}
		if err := db.RecordTurn(ctx, e, turn); err != nil {
			// Counted, not just logged. "Written" used to advance whether or
			// not the insert worked, so health reported a complete record over
			// a database that was rejecting every row.
			r.failed.Add(1)
			log.Error("event: not recorded", "role", ev.Role, "kind", ev.Kind, "err", err)
			continue
		}
		r.written.Add(1)
	}
}

// Payload is the per-kind detail the activity view renders, as JSON.
//
// Exported because a live event goes to the browser straight off the bus,
// without passing through the table, and both paths must produce the same
// shape — a client should not be able to tell them apart.
//
// Encoding failure returns nil rather than an error: an event that loses its
// arguments is still worth keeping, and a tool call with unencodable input
// should not remove the record that it happened.
func Payload(ev Event) json.RawMessage {
	var v any
	switch ev.Kind {
	case adapter.EventToolCall:
		if len(ev.Args) == 0 {
			return nil
		}
		v = ev.Args
	case adapter.EventUsage:
		// The input split is kept apart here for the same reason it is kept
		// apart in usage_turns: the three are priced roughly 50x from each
		// other, so one combined figure describes nothing.
		v = map[string]any{
			"in":         ev.TokensIn,
			"cacheRead":  ev.CacheReadTokens,
			"cacheWrite": ev.CacheWriteTokens,
			"out":        ev.TokensOut,
			"costUsd":    ev.CostUSD,
			"billing":    string(ev.Billing),
			"model":      ev.Model,
			"provider":   ev.Provider,
		}
	default:
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

func usageOf(ev Event, taskID *string) store.UsageTurn {
	// "computed" is reserved for a cost derived from a price table, and no such
	// table exists yet (§9's model_prices is unbuilt), so it is never written.
	// Labelling an unreported cost as computed would claim a calculation that
	// did not happen, and would make a stored 0.0 indistinguishable from a turn
	// that genuinely cost nothing.
	source := store.CostUnknown
	if ev.CostReported {
		source = store.CostFromHarness
	}
	return store.UsageTurn{
		ProjectID: ev.ProjectID, TaskID: taskID, Role: ev.Role, At: ev.At,
		Harness: ev.Harness, Provider: ev.Provider, Model: ev.Model,
		InputTokens:      ev.TokensIn,
		CacheWriteTokens: ev.CacheWriteTokens,
		CacheReadTokens:  ev.CacheReadTokens,
		OutputTokens:     ev.TokensOut,
		CostUSD:          ev.CostUSD,
		CostSource:       source,
		Billing:          string(ev.Billing),
	}
}
