package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/store"
)

// Ping cadence. An agent can think for a minute without emitting anything, so
// silence is normal and cannot be read as a dead peer. The ping is what tells
// the two apart, and it is also what keeps an intermediary from reaping an idle
// connection.
const (
	pingEvery   = 20 * time.Second
	pingTimeout = 10 * time.Second

	// A slow reader must never hold up a write. Failing the connection frees
	// the subscription and lets the client reconnect from its cursor, which is
	// strictly better than blocking the loop that serves everyone.
	writeTimeout = 10 * time.Second
)

// Frames the client sends.
type clientFrame struct {
	Type string `json:"type"`

	// After is the last event id the client holds. Replay resumes from it, so a
	// reconnect loses nothing — this is the cursor EventSource used to send as
	// Last-Event-ID, now carried explicitly because a socket has no equivalent.
	After string `json:"after,omitempty"`
	Role  string `json:"role,omitempty"`
	Task  string `json:"task,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// Frames the server sends. One envelope for every direction and kind, so adding
// a message later — a takeover keystroke, an answer to a clarification — is a
// new Type rather than a new endpoint.
type serverFrame struct {
	Type     string       `json:"type"`
	Event    *store.Event `json:"event,omitempty"`
	Replayed int          `json:"replayed,omitempty"`
	Message  string       `json:"message,omitempty"`
}

// stream serves a project's activity over a WebSocket.
//
// One duplex connection rather than SSE. The stream itself is one-directional
// today, and SSE would carry it with less machinery — reconnect and cursor
// resume come free with EventSource, and both are hand-rolled here. The trade
// is deliberate: terminal takeover (§10.1) needs keystrokes flowing back at
// typing latency, which SSE plus a POST per keypress serves badly, and running
// two streaming paths until then would mean two implementations of replay that
// drift apart. The envelope below is shaped for that second direction.
//
// It is also not subject to the six-connections-per-origin limit that applies
// to HTTP/1.1, which an SSE stream would hold open per tab.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.db.GetProject(r.Context(), projectID); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, "no event bus is attached")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The cockpit is served from this same origin. Anything else asking for
		// a live feed of what the agents are doing is not a browser tab of
		// this app, and the default same-origin check is exactly right.
		InsecureSkipVerify: false,
	})
	if err != nil {
		// Accept has already written a response.
		s.log.Debug("stream: upgrade refused", "err", err)
		return
	}
	defer conn.CloseNow()

	// Subscribe before reading history, so an event arriving mid-replay is
	// buffered rather than lost. The cost is that such an event is on both
	// paths; the cursor below sends it once.
	ch, unsubscribe := s.bus.Subscribe(1024)
	defer unsubscribe()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Client frames arrive on their own goroutine: the main loop must stay free
	// to write bus events, and a read blocks until something is sent.
	frames := make(chan clientFrame, 8)
	go func() {
		defer close(frames)
		for {
			var f clientFrame
			if err := wsjson.Read(ctx, conn, &f); err != nil {
				cancel() // a closed or broken read ends the connection
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	st := &streamState{}
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case f, open := <-frames:
			if !open {
				return
			}
			if f.Type != "subscribe" {
				continue // unknown frames are ignored, not fatal
			}
			if err := s.replay(ctx, conn, projectID, f, st); err != nil {
				s.log.Debug("stream: replay ended the connection", "err", err)
				return
			}

		case <-ping.C:
			// Ping rather than a comment frame: the protocol has a liveness
			// check built in, and a peer that fails it is genuinely gone.
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}

		case ev, open := <-ch:
			if !open {
				return
			}
			if !st.wants(ev, projectID) {
				continue
			}
			e := liveEvent(ev)
			if err := writeFrame(ctx, conn, serverFrame{Type: "activity", Event: &e}); err != nil {
				return
			}
			st.seen = ev.ID
		}
	}
}

// streamState is what this connection has asked for and how far it has got.
type streamState struct {
	subscribed bool
	role       string
	task       string

	// seen is the last id written. An event already sent during replay must not
	// be sent again when it also arrives on the bus.
	seen string
}

func (st *streamState) wants(ev event.Event, projectID string) bool {
	if !st.subscribed || ev.ProjectID != projectID {
		return false
	}
	if st.role != "" && ev.Role != st.role {
		return false
	}
	if st.seen != "" && ev.ID <= st.seen {
		return false
	}
	return true
}

// replay sends history, then marks the connection live.
func (s *Server) replay(ctx context.Context, conn *websocket.Conn,
	projectID string, f clientFrame, st *streamState) error {

	st.subscribed = true
	st.role, st.task = f.Role, f.Task

	history, err := s.db.ListEvents(ctx, store.EventQuery{
		ProjectID: projectID, After: f.After, Role: f.Role, Task: f.Task, Limit: f.Limit,
	})
	if err != nil {
		s.log.Error("stream: could not read history", "project", projectID, "err", err)
		// Reported in-band. Closing on silence would look to the client like a
		// project with no history rather than a query that failed.
		return writeFrame(ctx, conn, serverFrame{
			Type: "error", Message: "could not read history",
		})
	}

	for i := range history {
		if err := writeFrame(ctx, conn, serverFrame{Type: "activity", Event: &history[i]}); err != nil {
			return err
		}
		st.seen = history[i].ID
	}
	// The client can tell replay from live, so it knows when it is caught up
	// and can stop showing a connecting state.
	return writeFrame(ctx, conn, serverFrame{Type: "caught-up", Replayed: len(history)})
}

// writeFrame sends one frame under a deadline, so a reader that has stopped
// draining cannot pin the loop that serves every other connection.
func writeFrame(ctx context.Context, conn *websocket.Conn, f serverFrame) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(wctx, conn, f)
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

// PruneEvents runs the retention sweep on a timer.
//
// Events are the expensive tier — roughly 40 MB a day at five active roles —
// and the least valuable long-term: they exist to replay recent work. Costs,
// metrics and outcomes live in usage_turns and tasks and are unaffected.
//
// What was dropped is logged. A retention policy that trims silently is
// indistinguishable, to whoever reads the transcript later, from a complete
// record that happens to start on a Tuesday.
func PruneEvents(ctx context.Context, db *store.DB, log *slog.Logger, every time.Duration) {
	sweep := func() {
		// Read the window per sweep, not once at startup. Settings say
		// retention applies immediately, and it did not: the duration was
		// captured when the daemon started, so shortening it from days to hours
		// changed the number on the form and nothing on disk until the next
		// restart — with no way to tell from the outside which one was in
		// force.
		cfg, err := db.GetConfig(ctx)
		if err != nil {
			log.Error("events: could not read the retention setting", "err", err)
			return
		}
		window := cfg.Retention()
		if window <= 0 {
			return // keep everything
		}
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
