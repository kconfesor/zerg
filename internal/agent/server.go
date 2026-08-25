// Package agent is the surface agents talk to.
//
// Four verbs over a unix socket, authenticated by a token minted at spawn.
// The predecessor put helper scripts on each agent's PATH and had them infer
// their own identity from the working directory, which meant running one from
// a subdirectory silently created an empty queue there and reported no work.
// Here identity arrives in the environment and is never guessed.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/konfessor/zerg/internal/nydus"
	"github.com/konfessor/zerg/internal/store"
)

// pollInterval is how often a waiting agent re-checks the queue. Leases make
// this safe to keep simple: nothing is lost if a poll is missed, unlike a
// wake-up that had exactly one chance to be noticed.
const pollInterval = 250 * time.Millisecond

// Identity is what a token proves.
type Identity struct {
	ProjectID string
	Role      string
}

// Server answers agent calls on a unix socket.
type Server struct {
	db  *store.DB
	nyd *nydus.Nydus
	log *slog.Logger

	mu     sync.RWMutex
	tokens map[string]Identity

	listener net.Listener
	http     *http.Server
	path     string
}

func NewServer(db *store.DB, nyd *nydus.Nydus, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{db: db, nyd: nyd, log: log, tokens: map[string]Identity{}}
}

// Mint issues a token for one role and returns it.
//
// Tokens are per-spawn and role-scoped, so an agent cannot claim work for
// another role or send as one. The predecessor read the sender from an
// environment variable any agent could set to any value.
func (s *Server) Mint(projectID, role string) string {
	token := store.NewID()
	s.mu.Lock()
	s.tokens[token] = Identity{ProjectID: projectID, Role: role}
	s.mu.Unlock()
	return token
}

// Revoke invalidates a token, used when a role stops.
func (s *Server) Revoke(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

func (s *Server) identify(r *http.Request) (Identity, bool) {
	token := r.Header.Get("X-Zerg-Token")
	if token == "" {
		return Identity{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokens[token]
	return id, ok
}

// Listen starts the socket. path is removed first: a socket left by a previous
// run would otherwise make every start fail with "address already in use".
func (s *Server) Listen(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing the old socket at %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", path, err)
	}
	// The socket carries capability tokens; only this user may connect.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("securing %s: %w", path, err)
	}

	s.listener, s.path = ln, path
	s.http = &http.Server{Handler: s.routes()}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("agent socket stopped", "err", err)
		}
	}()
	return nil
}

// Path is the socket agents are pointed at.
func (s *Server) Path() string { return s.path }

func (s *Server) Close() error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	os.Remove(s.path)
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/next", s.next)
	mux.HandleFunc("POST /agent/done", s.done)
	mux.HandleFunc("POST /agent/send", s.send)
	mux.HandleFunc("POST /agent/ask", s.ask)
	return mux
}

// ── next ──────────────────────────────────────────────────────────────────

type nextRequest struct {
	WaitSeconds int `json:"waitSeconds"`
}

// NextResponse is what an agent receives when it claims work.
type NextResponse struct {
	LeaseID   string      `json:"leaseId"`
	Role      string      `json:"role"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Items     []Item      `json:"items"`
	Task      *store.Task `json:"task,omitempty"`

	// Next is the role this work goes to when finished, and Terminal says this
	// role finishes the task instead. Without these an agent has to guess a
	// recipient, and a wrong guess is rejected — which is exactly what
	// happened the first time a real agent ran this.
	Next     string `json:"next,omitempty"`
	Terminal bool   `json:"terminal"`
}

// Item is one unit of work inside a lease.
type Item struct {
	MessageID string  `json:"messageId"`
	From      string  `json:"from"`
	Kind      string  `json:"kind"`
	TaskID    *string `json:"taskId,omitempty"`
	TaskName  string  `json:"taskName,omitempty"`
	Commit    *string `json:"commit,omitempty"`
	Body      string  `json:"body"`

	// Merged states that the orchestrator already merged this commit into the
	// agent's worktree. The predecessor merged in its helper and *also* told
	// the agent to merge again in the payload; it worked only because the
	// second merge happened to be a no-op.
	Merged bool `json:"merged"`
}

// next claims work, waiting up to WaitSeconds for some to appear.
func (s *Server) next(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unrecognised token")
		return
	}

	var req nextRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // an empty body means do not wait

	deadline := time.Now()
	if req.WaitSeconds > 0 {
		deadline = deadline.Add(time.Duration(req.WaitSeconds) * time.Second)
	}

	for {
		lease, err := s.nyd.Claim(r.Context(), id.ProjectID, id.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		if lease != nil {
			out, err := s.describe(r.Context(), id, lease)
			if err != nil {
				s.fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		if !time.Now().Before(deadline) {
			// No work is not an error. An agent that is told "nothing yet"
			// should stop, not retry in a loop of its own devising.
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

func (s *Server) describe(ctx context.Context, id Identity, lease *store.Lease) (NextResponse, error) {
	out := NextResponse{
		LeaseID: lease.ID, Role: lease.Role, ExpiresAt: lease.ExpiresAt,
		Items: make([]Item, 0, len(lease.Items)),
	}

	// Where this work goes next, so the agent is told rather than guessing.
	team, err := s.db.ResolveTeam(ctx, id.ProjectID)
	if err != nil {
		return out, err
	}
	var enabled []store.ResolvedRole
	for _, r := range team {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	for i, r := range enabled {
		if r.Name != id.Role {
			continue
		}
		out.Terminal = r.Terminal
		if !r.Terminal && i+1 < len(enabled) {
			out.Next = enabled[i+1].Name
		}
	}
	for _, m := range lease.Items {
		item := Item{
			MessageID: m.ID, From: m.FromRole, Kind: m.Kind,
			TaskID: m.TaskID, Commit: m.CommitSHA, Body: m.Body,
			Merged: m.CommitSHA != nil,
		}
		if m.TaskID != nil {
			if task, err := s.db.GetTask(ctx, *m.TaskID); err == nil {
				item.TaskName = task.Name
				if out.Task == nil {
					out.Task = task
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// ── done ──────────────────────────────────────────────────────────────────

type doneRequest struct {
	LeaseID string `json:"leaseId"`
}

func (s *Server) done(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unrecognised token")
		return
	}
	var req doneRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LeaseID == "" {
		writeError(w, http.StatusBadRequest, "done needs a leaseId")
		return
	}
	if err := s.nyd.Ack(r.Context(), req.LeaseID); err != nil {
		s.fail(w, err)
		return
	}
	_ = id
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

// ── send ──────────────────────────────────────────────────────────────────

type sendRequest struct {
	To       string `json:"to"`
	TaskID   string `json:"taskId"`
	Kind     string `json:"kind"`
	Commit   string `json:"commit"`
	Body     string `json:"body"`
	Priority int    `json:"priority"`
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unrecognised token")
		return
	}
	var req sendRequest
	if !decode(w, r, &req) {
		return
	}

	// Agents are told the task name, and the name is what every handoff
	// carries, so --task accepts either form. Resolving it here also means a
	// wrong value reports itself rather than arriving as a foreign-key
	// violation from inside the router.
	if req.TaskID != "" {
		if _, err := s.db.GetTask(r.Context(), req.TaskID); err != nil {
			task, nameErr := s.db.GetTaskByName(r.Context(), id.ProjectID, req.TaskID)
			if nameErr != nil {
				writeError(w, http.StatusNotFound,
					fmt.Sprintf("no task with id or name %q in this project", req.TaskID))
				return
			}
			req.TaskID = task.ID
		}
	}

	// The sender is the token's role. There is no --from to forge.
	msg, err := s.nyd.Send(r.Context(), id.ProjectID, id.Role, nydus.SendRequest{
		To: req.To, TaskID: req.TaskID, Kind: req.Kind,
		Commit: req.Commit, Body: req.Body, Priority: req.Priority,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// ── ask ───────────────────────────────────────────────────────────────────

type askRequest struct {
	Question    string `json:"question"`
	TaskID      string `json:"taskId"`
	WaitSeconds int    `json:"waitSeconds"`
}

type askResponse struct {
	ID       string `json:"id"`
	Answer   string `json:"answer,omitempty"`
	Answered bool   `json:"answered"`
}

// ask raises a question and waits for a human.
//
// An agent that needs an answer must have somewhere to put the question. The
// predecessor forbade asking in the pane and offered a helper instead, so an
// unanswered question looked exactly like an agent that had stopped for no
// reason.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unrecognised token")
		return
	}
	var req askRequest
	if !decode(w, r, &req) {
		return
	}

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}
	c, err := s.db.AskClarification(r.Context(), id.ProjectID, id.Role, req.Question, taskID)
	if err != nil {
		s.fail(w, err)
		return
	}

	deadline := time.Now()
	if req.WaitSeconds > 0 {
		deadline = deadline.Add(time.Duration(req.WaitSeconds) * time.Second)
	}
	for {
		current, err := s.db.GetClarification(r.Context(), c.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if current.State == store.ClarificationAnswered && current.Answer != nil {
			writeJSON(w, http.StatusOK, askResponse{ID: c.ID, Answer: *current.Answer, Answered: true})
			return
		}
		if !time.Now().Before(deadline) {
			// Unanswered is reported, not hidden: the agent knows the question
			// is pending and can decide what to do rather than hanging.
			writeJSON(w, http.StatusOK, askResponse{ID: c.ID, Answered: false})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// ── plumbing ──────────────────────────────────────────────────────────────

func (s *Server) fail(w http.ResponseWriter, err error) {
	var v interface{ Validation() }
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &v):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("agent request failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("could not read the request: %v", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
