// Package agent is the surface agents talk to.
//
// Four verbs over a unix socket, authenticated by a token minted at spawn.
// Helper scripts that infer their own identity from the working directory mean
// running one from a subdirectory silently creates an empty queue there and
// reports no work. Here identity arrives in the environment and is never
// guessed.
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
	"strings"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

// pollInterval is how often a waiting agent re-checks the queue. Leases make
// this safe to keep simple: nothing is lost if a poll is missed, unlike a
// wake-up that had exactly one chance to be noticed.
const pollInterval = 250 * time.Millisecond

// Identity is what a token proves.
type Identity struct {
	ProjectID string
	Role      string

	// TaskID is the card this agent was spawned for, when it was spawned for
	// one rather than sent to claim work.
	//
	// A pipeline role finds its card through the lease it holds. An agent
	// given a job holds no lease, so what it produces had nowhere to attach:
	// a runner's service was recorded against the project and the card that
	// asked for it showed nothing. The daemon knew which card it was for at
	// the moment it started the agent, so the token carries it.
	TaskID string

	// Owner says whose processes these are: the swarm's, or the daemon's.
	//
	// A pipeline role's dev server dies when the swarm stops, because the
	// agent that started it is part of the swarm. A runner is not: the daemon
	// spawned it and it outlives Start and Stop, so the swarm going down must
	// leave its preview alone rather than marking a running service dead.
	Owner string

	// Can is what this token is allowed to call. Empty means everything, which
	// is what a pipeline role gets.
	//
	// Scopes exist because not every agent is a pipeline role. A runner starts
	// the project so somebody can look at it: it needs to register a service
	// and to ask a question, and it must never be able to put work into the
	// queue -- especially once it starts on its own when a task lands. A
	// capability that is not needed and not granted cannot be misused by an
	// agent that read the wrong file.
	Can map[string]bool
}

// The verbs a token can carry. A pipeline role holds all of them; anything
// spawned outside the pipeline holds the few it needs.
const (
	CanClaim    = "next"     // and done, which is the other half of holding a lease
	CanSend     = "send"     // put work into the queue
	CanAsk      = "ask"      // put a question to the operator
	CanArtifact = "artifact" // register what it produced
	CanRemember = "remember" // write down what it learned about this project
	CanDecide   = "decide"   // approve, reject, answer as the supervisor sidecar
)

// allows reports whether this identity may call a verb.
func (i Identity) allows(verb string) bool {
	// Decide is never implied. A pipeline token holds every other verb by
	// leaving Can empty; granting approve that way would let any coder land
	// a card, which is the same class of failure as a forgeable --from.
	if verb == CanDecide {
		return i.Can[verb]
	}
	if len(i.Can) == 0 {
		return true
	}
	return i.Can[verb]
}

// Server answers agent calls on a unix socket.
type Server struct {
	db  *store.DB
	nyd *nydus.Nydus
	log *slog.Logger

	// blobs is where a file artifact's bytes go. Nil in a daemon with nowhere
	// to put them, which answers the verb rather than crashing on it.
	blobs *artifact.Store

	mu     sync.RWMutex
	tokens map[string]Identity

	listener net.Listener
	http     *http.Server
	path     string

	// watch is told when an agent does something the cockpit's view of it
	// depends on. Optional, and set by the daemon rather than passed in: the
	// runner watches for its own agent registering a service or asking a
	// question, and it cannot be a constructor argument because the runner is
	// built on top of this package.
	watch Watcher
}

// Watcher hears about the two things that change what a waiting person sees.
type Watcher interface {
	// Served: this role registered a running service.
	Served(projectID, role string)
	// Asked: this role put a question to the operator and is waiting.
	Asked(projectID, role string)
}

// Watch sets the watcher. Called once, at startup.
func (s *Server) Watch(w Watcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watch = w
}

func (s *Server) watcher() Watcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watch
}

func NewServer(db *store.DB, nyd *nydus.Nydus, blobs *artifact.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{db: db, nyd: nyd, blobs: blobs, log: log, tokens: map[string]Identity{}}
}

// Mint issues a token for one role and returns it.
//
// Tokens are per-spawn and role-scoped, so an agent cannot claim work for
// another role or send as one. Reading the sender from an environment variable
// lets any agent set it to any value.
// Mint issues a token for a pipeline role: every verb, and whatever it starts
// belongs to the swarm.
func (s *Server) Mint(projectID, role string) string {
	return s.mint(Identity{ProjectID: projectID, Role: role})
}

// MintScoped issues a token limited to the verbs named.
func (s *Server) MintScoped(projectID, role string, can ...string) string {
	return s.mint(Identity{ProjectID: projectID, Role: role, Can: verbs(can)})
}

// MintFor issues a scoped token for an agent the daemon spawned to work on one
// card.
//
// Two things follow from being spawned rather than sent. What it produces
// attaches to that card, because it holds no lease to find one through. And
// what it starts belongs to the daemon rather than the swarm: Start and Stop
// are about the pipeline, and a preview somebody is looking at is not part of
// it.
func (s *Server) MintFor(projectID, role, taskID string, can ...string) string {
	return s.mint(Identity{
		ProjectID: projectID, Role: role, TaskID: taskID,
		Owner: store.OwnerDaemon, Can: verbs(can),
	})
}

func (s *Server) mint(id Identity) string {
	token := store.NewID()
	s.mu.Lock()
	s.tokens[token] = id
	s.mu.Unlock()
	return token
}

func verbs(can []string) map[string]bool {
	if len(can) == 0 {
		return nil
	}
	out := make(map[string]bool, len(can))
	for _, verb := range can {
		out[verb] = true
	}
	return out
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

// permit identifies the caller and checks it may call this verb.
//
// The refusal names the verb rather than saying "forbidden", because the agent
// reading it is deciding what to do next and "you cannot send work" is the
// sentence that tells it.
func (s *Server) permit(w http.ResponseWriter, r *http.Request, verb string) (Identity, bool) {
	id, ok := s.identify(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unrecognised token")
		return Identity{}, false
	}
	if !id.allows(verb) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("%s cannot %s: this agent was started to do one thing and that is not it",
				id.Role, verb))
		return Identity{}, false
	}
	return id, true
}

// Listen starts the socket. path is removed first: a socket left by a previous
// run would otherwise make every start fail with "address already in use".
func (s *Server) Listen(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	// Chmod as well as MkdirAll: the mode argument applies only when the
	// directory is created, so an installation that predates this kept the
	// 0755 it was made with — and this directory holds the socket that carries
	// capability tokens.
	//
	// Best effort, because the socket may legitimately live somewhere this
	// process does not own — /tmp, most obviously — and the socket file's own
	// 0600 is what actually keeps other users out.
	if err := os.Chmod(dir, 0o700); err != nil {
		s.log.Debug("could not tighten the socket directory", "dir", dir, "err", err)
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
	mux.HandleFunc("POST /agent/artifact", s.artifact)
	mux.HandleFunc("POST /agent/remember", s.remember)
	mux.HandleFunc("POST /agent/approve", s.approve)
	mux.HandleFunc("POST /agent/reject", s.reject)
	mux.HandleFunc("POST /agent/answer", s.answer)
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

	// Kind is "decide" when this is a judgement for the supervisor sidecar,
	// empty for ordinary pipeline work. A pipeline agent never sees decide.
	Kind            string   `json:"kind,omitempty"`
	ApprovalID      string   `json:"approvalId,omitempty"`
	ClarificationID string   `json:"clarificationId,omitempty"`
	Question        string   `json:"question,omitempty"`
	Options         []string `json:"options,omitempty"`
	From            string   `json:"from,omitempty"`
	Body            string   `json:"body,omitempty"`
	Commit          string   `json:"commit,omitempty"`
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

	// Merged says the orchestrator got this commit into the agent's worktree.
	// It comes from the merge attempt, never from the presence of a commit.
	// The first version derived it — `m.CommitSHA != nil` — while nothing
	// merged anything, so every hand-off asserted a merge that had not
	// happened and the instructions told the recipient not to repeat it. A
	// reviewer found its tree empty twice before working out it was being
	// lied to. False here means the merge conflicted or could not run, and
	// the agent completes it itself.
	Merged bool `json:"merged"`
}

// next claims work, waiting up to WaitSeconds for some to appear.
func (s *Server) next(w http.ResponseWriter, r *http.Request) {
	id, ok := s.permit(w, r, CanClaim)
	if !ok {
		return
	}

	var req nextRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // an empty body means do not wait

	deadline := time.Now()
	if req.WaitSeconds > 0 {
		deadline = deadline.Add(time.Duration(req.WaitSeconds) * time.Second)
	}

	for {
		if id.allows(CanDecide) {
			out, err := s.describeDecision(r.Context(), id)
			if err != nil {
				s.fail(w, err)
				return
			}
			if out != nil {
				writeJSON(w, http.StatusOK, out)
				return
			}
		} else {
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
	for _, m := range lease.Items {
		item := Item{
			MessageID: m.ID, From: m.FromRole, Kind: m.Kind,
			TaskID: m.TaskID, Commit: m.CommitSHA, Body: m.Body,
			Merged: lease.Merged[m.ID],
		}
		if m.TaskID != nil {
			// Not skipped on error. The card is the routing record -- it says
			// which roles this work does not visit -- so failing to read it and
			// carrying on means answering `next` and `terminal` from the full
			// team: telling an agent to hand work to a role the card skips, or
			// that it may finish a card it may not. A claim that cannot be
			// described is an error, and the agent retries.
			task, err := s.db.GetTask(ctx, *m.TaskID)
			if err != nil {
				return out, fmt.Errorf("reading task %s for lease %s: %w", *m.TaskID, lease.ID, err)
			}
			item.TaskName = task.Name
			if out.Task == nil {
				out.Task = task
			}
		}
		out.Items = append(out.Items, item)
	}

	// The route belongs to the card, not to the project: a card can be told to
	// skip a role, and then the role after it is what comes next and the last
	// one left is what finishes. One route per lease is what nydus.Claim
	// guarantees by refusing to batch cards that skip differently, so the task
	// read above answers for every item in it.
	var skip []string
	if out.Task != nil {
		skip = out.Task.Skip
	}
	route := store.Route(team, skip)
	out.Next, out.Terminal = store.Onward(team, route, id.Role)
	return out, nil
}

// ── done ──────────────────────────────────────────────────────────────────

type doneRequest struct {
	LeaseID string `json:"leaseId"`
}

func (s *Server) done(w http.ResponseWriter, r *http.Request) {
	id, ok := s.permit(w, r, CanClaim)
	if !ok {
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
	// The token's project and role, not just any lease id. A token proves who
	// is calling and the previous version then discarded that, acknowledging
	// whatever id it was handed — so any role could close any other role's
	// lease and return its unfinished work to the queue as done.
	if err := s.nyd.AckOwned(r.Context(), id.ProjectID, id.Role, req.LeaseID); err != nil {
		s.fail(w, err)
		return
	}
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
	id, ok := s.permit(w, r, CanSend)
	if !ok {
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
	var lease string
	if req.TaskID != "" {
		resolved, err := s.resolveTask(r.Context(), id.ProjectID, req.TaskID)
		if err != nil {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("no task with id or name %q in this project", req.TaskID))
			return
		}
		req.TaskID = resolved

		// And the role has to be holding it. Membership of the project was the
		// only check: a token for the terminal role could complete any card on
		// the board, including one another role was working at that moment, and
		// a token still valid from an earlier spawn could finish work it had
		// never been handed.
		//
		// The lease also scopes the idempotency key, so a send that is retried
		// after a lost response is absorbed rather than producing a second
		// hand-off. Found here, from the daemon's own records, rather than
		// asked of the agent — a value the caller supplies is a value the
		// caller can get wrong.
		held, err := s.nyd.LeaseHolding(r.Context(), id.ProjectID, id.Role, req.TaskID)
		if err != nil {
			s.fail(w, err)
			return
		}
		lease = held.ID
	}

	// The sender is the token's role. There is no --from to forge.
	msg, err := s.nyd.Send(r.Context(), id.ProjectID, id.Role, nydus.SendRequest{
		To: req.To, TaskID: req.TaskID, Kind: req.Kind,
		Commit: req.Commit, Body: req.Body, Priority: req.Priority,
		SourceLease: lease,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// ── ask ───────────────────────────────────────────────────────────────────

type askRequest struct {
	Question string `json:"question"`
	// Options turn the question into a choice. The operator picks one and the
	// answer comes back as that option's text, so an agent that offered
	// options can compare the answer to them rather than parse prose.
	Options     []string `json:"options,omitempty"`
	TaskID      string   `json:"taskId"`
	WaitSeconds int      `json:"waitSeconds"`
}

type askResponse struct {
	ID       string `json:"id"`
	Answer   string `json:"answer,omitempty"`
	Answered bool   `json:"answered"`
}

// ask raises a question and waits for a human.
//
// An agent that needs an answer must have somewhere to put the question.
// Forbid asking in the pane, offer a helper instead, and an unanswered question
// looks exactly like an agent that stopped for no reason.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.permit(w, r, CanAsk)
	if !ok {
		return
	}
	var req askRequest
	if !decode(w, r, &req) {
		return
	}

	// A question is filed against a card, so the card has to be one of this
	// project's. Unvalidated, an agent could attach its question to another
	// project's task and put it in the wrong operator's queue.
	var taskID *string
	if req.TaskID != "" {
		resolved, err := s.resolveTask(r.Context(), id.ProjectID, req.TaskID)
		if err != nil {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("no task with id or name %q in this project", req.TaskID))
			return
		}
		taskID = &resolved
	} else if held, err := s.db.CurrentTaskFor(r.Context(), id.ProjectID, id.Role); err == nil && held != nil {
		// No --task given: use the card this role is holding a lease on.
		//
		// The daemon knows which one that is, and the agent has to remember to
		// say. When it forgets, the question reaches the queue attached to
		// nothing — the bell counts it and no card shows it, so on a board with
		// several tasks there is no way to see which one is blocked. Inferring
		// it here is the same principle as resolving a commit in the sender's
		// worktree rather than trusting what was passed.
		taskID = held
	} else if id.TaskID != "" {
		// Spawned for a card rather than sent to claim one: the token carries
		// it, so a runner's question reaches the card that asked for the run
		// instead of the bell with nothing behind it.
		task := id.TaskID
		taskID = &task
	}
	c, err := s.db.AskClarification(r.Context(), id.ProjectID, id.Role, req.Question, req.Options, taskID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Waiting on a person, which a panel showing "working…" would otherwise
	// render as a build that has stalled.
	if wch := s.watcher(); wch != nil {
		wch.Asked(id.ProjectID, id.Role)
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
			// Read, so a later repeat of the question is a new question rather
			// than this answer handed over twice.
			if err := s.db.MarkClarificationDelivered(r.Context(), c.ID); err != nil {
				s.fail(w, err)
				return
			}
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

func (s *Server) describeDecision(ctx context.Context, id Identity) (*NextResponse, error) {
	d, err := s.db.NextDecision(ctx, id.ProjectID, id.Role)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, nil
	}
	out := NextResponse{Kind: "decide", Role: id.Role, Terminal: false}
	if d.Approval != nil {
		out.ApprovalID = d.Approval.ID
		out.From = d.Approval.FromRole
		out.Body = d.Approval.Body
		out.Commit = d.Approval.Commit
		if d.Approval.TaskID != "" {
			task, err := s.db.GetTask(ctx, d.Approval.TaskID)
			if err != nil {
				return nil, err
			}
			out.Task = task
		}
	}
	if d.Clarification != nil {
		out.ClarificationID = d.Clarification.ID
		out.Question = d.Clarification.Question
		out.Options = d.Clarification.Options
		out.From = d.Clarification.Role
		if d.Clarification.TaskID != nil {
			task, err := s.db.GetTask(ctx, *d.Clarification.TaskID)
			if err != nil {
				return nil, err
			}
			out.Task = task
		}
	}
	return &out, nil
}

type decideRequest struct {
	ID     string `json:"id"`
	Note   string `json:"note"`
	Answer string `json:"answer"`
	Commit string `json:"commit"`
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, true)
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	s.decideApproval(w, r, false)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, ok bool) {
	id, permitted := s.permit(w, r, CanDecide)
	if !permitted {
		return
	}
	var req decideRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "an approval needs --id, from the decide envelope")
		return
	}
	if !ok && strings.TrimSpace(req.Note) == "" {
		writeError(w, http.StatusBadRequest, "a rejection needs --note: what to change")
		return
	}
	if ok && strings.TrimSpace(req.Note) == "" {
		writeError(w, http.StatusBadRequest, "an approval needs --note: the rationale")
		return
	}
	scope := store.DecisionScope{ProjectID: id.ProjectID, Role: id.Role}
	var err error
	if ok {
		err = s.nyd.ApproveBy(r.Context(), scope, req.ID, req.Note, req.Commit)
	} else {
		err = s.nyd.RejectBy(r.Context(), scope, req.ID, req.Note, req.Commit)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": map[bool]string{true: "approved", false: "rejected"}[ok]})
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	id, permitted := s.permit(w, r, CanDecide)
	if !permitted {
		return
	}
	var req decideRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "an answer needs --id, from the decide envelope")
		return
	}
	if strings.TrimSpace(req.Answer) == "" {
		writeError(w, http.StatusBadRequest, "an answer cannot be empty")
		return
	}
	// Scope, supervision and authorship are checked at the write, in one
	// place, rather than partly here: this handler used to check the project
	// and nothing else, so a sidecar could answer a question on an
	// unsupervised card, or the one it had asked itself.
	scope := store.DecisionScope{ProjectID: id.ProjectID, Role: id.Role}
	if err := s.nyd.AnswerBy(r.Context(), scope, req.ID, req.Answer, req.Commit); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "answered"})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// resolveTask turns whatever an agent passed as --task into an id belonging to
// that agent's own project.
//
// Agents are told the task name, and the name is what every handoff carries,
// so either form is accepted. Both are looked up inside the project: a global
// lookup would let one project's agent name another's card, and the id would
// then flow into routing and task updates unchecked.
func (s *Server) resolveTask(ctx context.Context, projectID, ref string) (string, error) {
	if t, err := s.db.GetTaskIn(ctx, projectID, ref); err == nil {
		return t.ID, nil
	}
	t, err := s.db.GetTaskByName(ctx, projectID, ref)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}
