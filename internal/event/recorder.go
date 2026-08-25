package event

import (
	"context"
	"log/slog"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/store"
)

// RecordUsage persists every usage event.
//
// Harnesses report tokens and cost per turn and the numbers were going only to
// a log line, so a finished task left no record of what it cost. That is the
// data the dashboard and every historical chart are built on, and it cannot be
// reconstructed after the fact — an unrecorded turn is unrecorded forever.
//
// A recorder is a subscriber like any other: it cannot slow an agent down, and
// a write that fails is logged rather than retried. Losing a usage row is a gap
// in a chart; blocking the bus to avoid one would stall the run that produced
// it.
func RecordUsage(ctx context.Context, bus *Bus, db *store.DB, log *slog.Logger) {
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
				if ev.Kind != adapter.EventUsage || ev.ProjectID == "" {
					continue
				}
				record(ctx, db, log, ev)
			}
		}
	}()
}

func record(ctx context.Context, db *store.DB, log *slog.Logger, ev Event) {
	// Which card the tokens were spent on. A miss is not worth abandoning the
	// row over: project-level spend is still correct without it, and a turn
	// attributed to no task is better than a turn recorded nowhere.
	taskID, err := db.CurrentTaskFor(ctx, ev.ProjectID, ev.Role)
	if err != nil {
		log.Debug("usage: could not attribute to a task", "role", ev.Role, "err", err)
	}

	source := "computed"
	if ev.CostReported {
		source = "harness"
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
