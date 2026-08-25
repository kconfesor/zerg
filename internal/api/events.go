package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/store"
)

// heartbeat keeps an idle stream from being closed by an intermediary. An agent
// can think for a minute without emitting anything, which is indistinguishable
// from a dead connection to anything sitting in between.
const heartbeat = 20 * time.Second

// events streams a project's activity as Server-Sent Events.
//
// SSE rather than a WebSocket because this stream only ever goes one way. The
// return path is the only thing a WebSocket buys, and it costs a dependency and
// a hand-rolled reconnect. EventSource reconnects on its own and resends the
// last id it saw as Last-Event-ID — and since event ids are monotonic ULIDs,
// that header is already a valid replay cursor. Resume is exact, for free.
//
// The response is replay-then-tail: history up to now, then live events, with
// no gap between them. The subscription opens *before* the replay query runs,
// so an event arriving mid-replay is buffered rather than lost — closing that
// window the other way round would drop exactly the events a busy project
// produces most.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.db.GetProject(r.Context(), projectID); err != nil {
		s.fail(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}

	q := r.URL.Query()
	// Last-Event-ID is what the browser resends on an automatic reconnect; the
	// query parameter is for a caller driving the stream itself. The header
	// wins, because a reconnect knows better than the URL it was opened with.
	after := q.Get("after")
	if resume := r.Header.Get("Last-Event-ID"); resume != "" {
		after = resume
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	role, task := q.Get("role"), q.Get("task")

	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, "no event bus is attached")
		return
	}
	ch, unsubscribe := s.bus.Subscribe(1024)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Buffering proxies defeat streaming entirely, and do it silently.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	history, err := s.db.ListEvents(r.Context(), store.EventQuery{
		ProjectID: projectID, After: after, Role: role, Task: task, Limit: limit,
	})
	if err != nil {
		// The headers are already out, so this cannot become a status code.
		// Say so in the stream instead of closing on a silence the client
		// would read as "nothing happened here".
		s.log.Error("events: replay failed", "project", projectID, "err", err)
		writeSSE(w, flusher, "error", "", []byte(`{"message":"could not read history"}`))
		return
	}

	seen := ""
	for _, e := range history {
		if !writeEvent(w, flusher, e) {
			return
		}
		seen = e.ID
	}
	// The client can tell replay from live, so it knows when it is caught up.
	writeSSE(w, flusher, "caught-up", "", []byte(fmt.Sprintf(`{"replayed":%d}`, len(history))))

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-ticker.C:
			// A comment line: valid SSE, ignored by EventSource, enough to keep
			// the connection from being reaped.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case ev, open := <-ch:
			if !open {
				return
			}
			if ev.ProjectID != projectID {
				continue
			}
			if role != "" && ev.Role != role {
				continue
			}
			// An event already sent during replay must not be sent again. The
			// subscription opened first on purpose, so this overlap is
			// expected rather than exceptional.
			if seen != "" && ev.ID <= seen {
				continue
			}
			if !writeEvent(w, flusher, liveEvent(ev)) {
				return
			}
			seen = ev.ID
		}
	}
}

// liveEvent shapes a bus event like a stored one, so a client parses one format
// whether the event came from the table or the wire.
func liveEvent(ev event.Event) store.Event {
	return store.Event{
		ID: ev.ID, ProjectID: ev.ProjectID, Role: ev.Role,
		Kind: string(ev.Kind), At: ev.At,
		Text: ev.Text, Tool: ev.Tool, Fatal: ev.Fatal,
		Data: event.Payload(ev),
	}
}

func writeEvent(w http.ResponseWriter, f http.Flusher, e store.Event) bool {
	body, err := json.Marshal(e)
	if err != nil {
		return true // skip one unencodable event rather than end the stream
	}
	return writeSSE(w, f, "activity", e.ID, body)
}

// writeSSE emits one frame and reports whether the connection is still usable.
func writeSSE(w http.ResponseWriter, f http.Flusher, name, id string, data []byte) bool {
	if id != "" {
		// The id field is what the browser stores and resends on reconnect.
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return false
	}
	f.Flush()
	return true
}

// pruneEvents runs the retention sweep on a timer.
//
// Events are the expensive tier — roughly 40 MB a day at five active roles —
// and the least valuable long-term: they exist to replay recent work. Costs,
// metrics and outcomes live in usage_turns and tasks and are unaffected.
//
// What was dropped is logged. A retention policy that trims silently is
// indistinguishable, to whoever reads the transcript later, from a complete
// record that happens to start on a Tuesday.
func PruneEvents(ctx context.Context, db *store.DB, log *slog.Logger, window, every time.Duration) {
	sweep := func() {
		n, err := db.PruneEvents(ctx, time.Now().Add(-window))
		if err != nil {
			log.Error("events: retention sweep failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("events: pruned", "rows", n, "older_than", window.String())
		}
	}

	go func() {
		sweep() // once at startup, so a long-stopped daemon does not carry a backlog
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}
