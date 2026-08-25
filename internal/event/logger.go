package event

import (
	"context"
	"log/slog"
)

// LogEvents writes the event stream to a logger.
//
// Until events are persisted and rendered, this is the only way to see what an
// agent is doing — and an orchestrator whose agents are unobservable is the
// exact failure it exists to prevent. Tool calls log at debug; anything an
// operator would want to know about logs at info or above.
func LogEvents(ctx context.Context, bus *Bus, log *slog.Logger) {
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
				logOne(log, ev)
			}
		}
	}()
}

func logOne(log *slog.Logger, ev Event) {
	base := []any{"role", ev.Role, "kind", string(ev.Kind)}

	switch ev.Kind {
	case "error":
		args := append(base, "text", ev.Text, "fatal", ev.Fatal)
		log.Error("agent", args...)
	case "tool_call":
		args := append(base, "tool", ev.Tool)
		if cmd, ok := ev.Args["command"].(string); ok {
			args = append(args, "command", truncate(cmd, 160))
		}
		if path, ok := ev.Args["file_path"].(string); ok {
			args = append(args, "file", path)
		}
		log.Info("agent", args...)
	case "usage":
		log.Info("agent", append(base,
			"in", ev.TokensIn, "cache_read", ev.CacheReadTokens,
			"cache_write", ev.CacheWriteTokens, "out", ev.TokensOut,
			"cost_usd", ev.CostUSD, "billing", string(ev.Billing))...)
	case "message":
		log.Info("agent", append(base, "text", truncate(ev.Text, 200))...)
	default:
		log.Info("agent", base...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
