// Package runner starts a project so a person can look at it.
//
// The third kind of agent here. A pipeline role claims cards, hands work on
// and lives on the board; chat is project-scoped, holds no lease and produces
// no cards. A runner is chat's shape with a job: given a commit, work out how
// this project serves itself, start it, and register the port.
//
// It is deliberately not a task. There is no card, no lane, no lease and no
// rework counter, because "run this so I can see it" is not work flowing
// through a pipeline: it is about another card, it never lands a commit, and
// putting it on the board would make it look like product work.
//
// What it does not know, it works out. What it works out, it writes down. What
// it cannot work out, it asks — the same clarification that any role asks,
// landing in Attention against the card being looked at.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// Role is the name a runner's events and artifacts carry.
//
// A name rather than a flag, like chat's: the existing role filter, colour
// assignment and replay all work on it with no special case. It is not a team
// role and never appears in a pipeline.
const Role = "runner"

// Idle is how long a preview nobody is touching stays up.
//
// Touching means pressing run, guiding it, answering its question or opening
// the frame. Reading the panel does not count, or a tab left open would keep
// an agent and a dev server alive for ever.
const Idle = 30 * time.Minute

// State is what the cockpit shows.
const (
	// StateWorking: the agent is reading the repository and trying things.
	StateWorking = "working"
	// StateServing: a port is registered and the frame works.
	StateServing = "serving"
	// StateAsking: it asked a question and is waiting for an answer.
	StateAsking = "asking"
	// StateGaveUp: the turn ended without anything serving.
	StateGaveUp = "gave up"
	// StateIdle: nothing is running.
	StateIdle = "idle"
)

// systemPrompt is the instruction, not a command.
//
// Everything specific to a project is discovered by reading it. The three
// rules that matter are here: bind the port you were given, register it, and
// ask rather than guess -- an agent that guesses which of three apps to serve
// wastes a turn and shows the wrong thing, which is worse than a question.
const systemPrompt = `You are starting this project so a person can look at it in a browser.

The repository is checked out at the commit being reviewed. Work out how this
project serves itself and start it. Read what is actually here: compose files,
package scripts, a justfile or Makefile, the README, how the app is configured.

Rules:

  Bind $PORT. It is set in your environment and the daemon is proxying it. A
  server on any other port cannot be reached and does not count as started.

  Start it in the background and leave it running. Your turn ends; the server
  must not end with it. Then register it:

      zerg artifact serve --port $PORT --label "<what it is>"

  Say what you learned, so the next run does not repeat the search:

      zerg remember "serves with: <command>. needs: <what, if anything>.
                     takes about <n> seconds to be ready."

  Ask rather than guess. If the project needs a file that is not in the
  repository, a secret, a database, or if there are several apps and no way to
  tell which one is wanted:

      zerg ask "which of these should I serve: admin, customer, or the API?"

  It blocks until somebody answers, and the answer is worth remembering too.

  If it will not start, say why in a sentence and stop. Do not rewrite the
  project to make it start: you are showing what is there, not fixing it.

You cannot claim work, hand work on, or finish a task. Those verbs are not
yours; this is the whole of your job.`

// Manager owns one runner session per project.
type Manager struct {
	db       *store.DB
	registry *adapter.Registry
	bus      *event.Bus
	agents   *agent.Server
	log      *slog.Logger

	// binDir holds the zerg the agent is told to run, prepended to its PATH.
	//
	// Without it the runner cannot find its own CLI: the first real session
	// spent part of a turn hunting for the binary and wrote "zerg CLI binary
	// path not on PATH -- found at ~/.zerg/state/bin/zerg" into its note,
	// which is a fact about this daemon's wiring leaking into what it learned
	// about the project.
	binDir string

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	cer    *cerebrate.Cerebrate
	cancel context.CancelFunc
	token  string
	port   int

	// What the cockpit reads.
	state   string
	message string // why it gave up, when it did
	commit  string
	taskID  string
	touched time.Time
}

func NewManager(db *store.DB, reg *adapter.Registry, bus *event.Bus, agents *agent.Server,
	binDir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{db: db, registry: reg, bus: bus, agents: agents, binDir: binDir, log: log,
		sessions: map[string]*session{}}
	go m.reapIdle()
	go m.watchTurns()
	return m
}

// watchTurns settles the state when the agent stops talking.
//
// Nothing else can. A turn that ends having registered a service is serving; a
// turn that ends without one has given up, and saying so is the difference
// between a panel that reports a failure and one that shows "working" until
// the idle timer collects it half an hour later.
func (m *Manager) watchTurns() {
	events, cancel := m.bus.Subscribe(256)
	defer cancel()
	for ev := range events {
		if ev.Role != Role || ev.Kind != adapter.EventTurnEnd {
			continue
		}
		live, err := m.db.LiveServices(context.Background(), ev.ProjectID)
		if err != nil {
			m.log.Warn("could not read the project's services", "project", ev.ProjectID, "err", err)
			continue
		}
		if len(live) > 0 {
			m.setState(ev.ProjectID, StateServing, "")
			continue
		}
		m.setState(ev.ProjectID, StateGaveUp,
			"the agent finished without starting anything; read what it said, or tell it something")
	}
}

// Run starts, or restarts, a project's preview at a commit.
//
// Restarting rather than queueing: one preview per project, and the question
// is always about the commit in front of somebody. A second press replaces
// what was there.
func (m *Manager) Run(ctx context.Context, projectID, commit, taskID string) error {
	if commit == "" {
		return fmt.Errorf("a preview needs the commit to run")
	}
	m.Stop(ctx, projectID)

	project, err := m.db.GetProject(ctx, projectID)
	if err != nil {
		return err
	}

	role, err := m.pickHarness(ctx, project)
	if err != nil {
		return err
	}
	ad, err := m.registry.Get(role.Harness)
	if err != nil {
		return err
	}

	// The commit, detached, in a worktree of its own. Detached because this is
	// a copy to run rather than a branch to work on, and its own because a
	// build must never touch the operator's checkout -- and because the commit
	// being reviewed has not merged and may never.
	hat := hatchery.New(project.Path)
	worktree, err := hat.PreviewWorktree(ctx, commit)
	if err != nil {
		return err
	}

	// Three verbs and no more. It registers what it started, asks when it is
	// stuck, and writes down what it learned. It cannot claim work or hand any
	// on -- which matters most when it starts on its own, after a task lands.
	token := m.agents.MintFor(projectID, Role, taskID,
		agent.CanArtifact, agent.CanAsk, agent.CanRemember)

	// The port is the daemon's to choose, not the project's. Two projects that
	// both default to 5173 would collide, and the proxy has to know where to
	// send traffic before the agent has decided anything.
	port, err := freePort()
	if err != nil {
		return err
	}

	role.Name = Role
	role.Prompt = systemPrompt

	cer := cerebrate.New(cerebrate.Config{
		ProjectID: projectID,
		Role:      role,
		Adapter:   ad,
		Worktree:  worktree,
		Socket:    m.agents.Path(),
		Token:     token,
		BinDir:    m.binDir,
		Env: []string{
			fmt.Sprintf("PORT=%d", port),
			fmt.Sprintf("ZERG_COMMIT=%s", commit),
		},
		Bus:          m.bus,
		Log:          m.log,
		SystemPrompt: systemPrompt,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := cer.Run(runCtx); err != nil && runCtx.Err() == nil {
			m.log.Error("runner session ended", "project", projectID, "err", err)
		}
	}()

	s := &session{
		cer: cer, cancel: cancel, token: token, port: port,
		state: StateWorking, commit: commit, taskID: taskID, touched: time.Now(),
	}
	m.mu.Lock()
	m.sessions[projectID] = s
	m.mu.Unlock()

	if err := cer.WaitReady(ctx); err != nil {
		m.Stop(ctx, projectID)
		return fmt.Errorf("the runner did not start: %w", err)
	}
	if err := cer.Submit(m.instruction(ctx, projectID, commit)); err != nil {
		m.Stop(ctx, projectID)
		return err
	}
	m.log.Info("runner started", "project", projectID, "commit", commit)
	return nil
}

// Guide sends a correction to the running session.
//
// The reason the session stays alive. "No, the admin portal" reaches an agent
// that still remembers what it just tried, which is both cheaper and more
// likely to work than starting again from nothing.
func (m *Manager) Guide(ctx context.Context, projectID, text string) error {
	m.mu.Lock()
	s, ok := m.sessions[projectID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("nothing is running to tell")
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("say what to change")
	}
	m.touch(projectID)
	m.setState(projectID, StateWorking, "")
	return s.cer.Submit(text)
}

// Stop ends a project's runner and everything it started.
func (m *Manager) Stop(ctx context.Context, projectID string) {
	m.mu.Lock()
	s, ok := m.sessions[projectID]
	delete(m.sessions, projectID)
	m.mu.Unlock()
	if !ok {
		return
	}

	// The token dies with it. A capability outliving the process that held it
	// is a capability nobody is watching.
	m.agents.Revoke(s.token)
	s.cancel()

	// Whatever it registered is gone with its process: the agent is the parent
	// of the server it started.
	if _, err := m.db.StopServices(ctx, projectID, store.OwnerDaemon); err != nil {
		m.log.Warn("could not mark the preview stopped", "project", projectID, "err", err)
	}
	m.log.Info("runner stopped", "project", projectID)
}

// StopAll ends every session, for a daemon shutting down.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(ctx, id)
	}
}

// Status is what the cockpit reads.
type Status struct {
	State   string `json:"state"`
	Commit  string `json:"commit,omitempty"`
	TaskID  string `json:"taskId,omitempty"`
	Message string `json:"message,omitempty"`
	// Since is when this state began, so the panel can say how long a build
	// has been going rather than only that it is going.
	Since time.Time `json:"since,omitempty"`
}

func (m *Manager) Status(projectID string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[projectID]
	if !ok {
		return Status{State: StateIdle}
	}
	return Status{State: s.state, Commit: s.commit, TaskID: s.taskID,
		Message: s.message, Since: s.touched}
}

// Served and Asked implement agent.Watcher: the socket tells this manager when
// its own agent got somewhere. Other roles do the same things for their own
// reasons, so the role is checked rather than assumed.
func (m *Manager) Served(projectID, role string) {
	if role == Role {
		m.setState(projectID, StateServing, "")
	}
}

func (m *Manager) Asked(projectID, role string) {
	if role == Role {
		m.setState(projectID, StateAsking, "")
	}
}

// Touch marks interest, which is what the idle timer measures.
func (m *Manager) Touch(projectID string) { m.touch(projectID) }

func (m *Manager) touch(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[projectID]; ok {
		s.touched = time.Now()
	}
}

func (m *Manager) setState(projectID, state, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[projectID]; ok {
		s.state, s.message = state, message
		s.touched = time.Now()
	}
}

// instruction is what the session is told to do, with what a previous one
// learned in front of it.
func (m *Manager) instruction(ctx context.Context, projectID, commit string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Start this project so somebody can look at it. You are on commit %s.\n", commit)

	if note, err := m.db.RunNoteFor(ctx, projectID); err == nil && note.Note != "" {
		// What was worked out before, offered rather than imposed: the
		// repository is the authority and it may have moved since.
		b.WriteString("\nWhat was learned about this project last time")
		if note.Author != Role {
			b.WriteString(", corrected by the operator")
		}
		b.WriteString(":\n\n")
		b.WriteString(note.Note)
		b.WriteString("\n\nTry that first. If it no longer fits what is in the tree, work it out " +
			"again and write down what changed.\n")
	} else {
		b.WriteString("\nNothing has been written down about this project yet, so read it and " +
			"work it out, then write down what worked.\n")
	}
	return b.String()
}

// pickHarness decides which agent runs this, the same way chat does: an
// explicit choice, otherwise the team's last enabled role.
func (m *Manager) pickHarness(ctx context.Context, project *store.Project) (store.ResolvedRole, error) {
	team, err := m.db.ResolveTeam(ctx, project.ID)
	if err != nil {
		return store.ResolvedRole{}, err
	}
	var pick store.ResolvedRole
	for _, r := range team {
		if r.Enabled {
			pick = r
		}
	}
	if project.ChatHarness != "" {
		pick.Harness = project.ChatHarness
		pick.Model = project.ChatModel
	} else if project.ChatModel != "" {
		pick.Model = project.ChatModel
	}
	if pick.Harness == "" {
		return store.ResolvedRole{}, fmt.Errorf(
			"no harness to run with: this project has no enabled roles, and no chat harness is set")
	}
	return pick, nil
}

// freePort asks the operating system for one nothing is using.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a port for the preview: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// reapIdle stops sessions nobody has touched.
//
// A preview holds an agent process and a dev server. Left alone, a tab open
// since yesterday would keep both alive and, on a metered harness, keep
// costing.
func (m *Manager) reapIdle() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		var stale []string
		for id, s := range m.sessions {
			if time.Since(s.touched) > Idle {
				stale = append(stale, id)
			}
		}
		m.mu.Unlock()
		for _, id := range stale {
			m.log.Info("runner idle, stopping", "project", id, "after", Idle.String())
			m.Stop(context.Background(), id)
		}
	}
}
