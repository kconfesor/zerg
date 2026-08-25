package event

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/store"
)

// Record persists the event stream: every event for replay, and usage
// separately for cost.
//
// Both come off one subscription rather than two, so a usage event resolves
// which card it belongs to once instead of twice, and the two records can never
// disagree about the order they arrived in.
//
// What it must not do is slow an agent down. A recorder is an ordinary bus
// subscriber — a write that fails is logged, never retried, and never blocks.
// A missing row is a gap in a transcript; blocking the bus to avoid one would
// stall the run that produced it.
func Record(ctx context.Context, bus *Bus, db *store.DB, log *slog.Logger) {
	ch, cancel := bus.Subscribe(1024)
	go func() {
		defer cancel()
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
				// Which card the work belonged to, through the role's lease. A
				// miss is ordinary — agents emit events before claiming
				// anything — and never a reason to drop the row.
				//
				// This is a three-table join per event. At observed volumes
				// (~20 events per turn) that is nothing against a local WAL
				// database; if it ever shows up, the fix is a short-TTL cache
				// keyed by role, not dropping the attribution.
				taskID, err := db.CurrentTaskFor(ctx, ev.ProjectID, ev.Role)
				if err != nil {
					log.Debug("event: could not attribute to a task",
						"role", ev.Role, "err", err)
				}

				recordEvent(ctx, db, log, ev, taskID)
				if ev.Kind == adapter.EventUsage {
					recordUsage(ctx, db, log, ev, taskID)
				}
			}
		}
	}()
}

func recordEvent(ctx context.Context, db *store.DB, log *slog.Logger, ev Event, taskID *string) {
	e := &store.Event{
		// The id was assigned at publish and is a monotonic ULID, so the value
		// the browser holds as Last-Event-ID is the same value stored here.
		// Reusing it is what makes resume-after-reconnect exact.
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
	if err := db.RecordEvent(ctx, e); err != nil {
		log.Error("event: not recorded", "role", ev.Role, "kind", ev.Kind, "err", err)
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

func recordUsage(ctx context.Context, db *store.DB, log *slog.Logger, ev Event, taskID *string) {
	// "computed" is reserved for a cost derived from a price table, and no such
	// table exists yet (§9's model_prices is unbuilt), so it is never written.
	// Labelling an unreported cost as computed would claim a calculation that
	// did not happen, and would make a stored 0.0 indistinguishable from a turn
	// that genuinely cost nothing.
	source := store.CostUnknown
	if ev.CostReported {
		source = store.CostFromHarness
	}
	if err := db.RecordUsage(ctx, store.UsageTurn{
		ProjectID: ev.ProjectID, TaskID: taskID, Role: ev.Role, At: ev.At,
		Provider: ev.Provider, Model: ev.Model,
		InputTokens:      ev.TokensIn,
		CacheWriteTokens: ev.CacheWriteTokens,
		CacheReadTokens:  ev.CacheReadTokens,
		OutputTokens:     ev.TokensOut,
		CostUSD:          ev.CostUSD,
		CostSource:       source,
		Billing:          string(ev.Billing),
	}); err != nil {
		log.Error("usage: not recorded", "role", ev.Role, "err", err)
	}
}
