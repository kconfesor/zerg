// Package chat runs a conversation with an agent that has the project in front
// of it but no part in the pipeline.
//
// Deliberately separate from the roles doing work. A question asked of a busy
// coder would interleave with its turn, and answering it would spend the
// context that agent needs for the task it is holding — so chat gets its own
// process, its own worktree, and no capability token at all. It cannot claim
// work, hand work on, or be handed any.
//
// Its messages are ordinary bus events, which is what makes the conversation
// persist, replay after a reload and stream over the same socket as everything
// else. There is no second history to keep in sync.
package chat

import (
	"context"
	"fmt"
	"sync"

	"log/slog"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/cerebrate"
	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/hatchery"
	"github.com/konfessor/zerg/internal/store"
)

// Role is the name chat events carry, and the worktree they run in.
//
// A name rather than a flag on the event, so the existing role filter, colour
// assignment and replay all work on it with no special case.
const Role = "chat"

// Operator is the name the person's own messages carry, so a conversation
// reads as a conversation rather than a monologue with gaps.
const Operator = "operator"

// systemPrompt replaces the shared instructions entirely.
//
// The protocol document tells a role how to claim work and hand it on, and
// every line of it would be a lie here: there is no socket to call and no queue
// to claim from. An agent told to run `zerg next` with no way to do so spends a
// turn discovering that.
const systemPrompt = `You are answering questions about the repository you are in.

You have read access to the project and the ordinary tools. Answer from what is
actually in the tree rather than from what a project like this usually contains
— read the files before describing them.

You are not doing the work. If the answer is "this needs a change", say what the
change would be; someone will queue it as a task. Do not edit files unless you
are explicitly asked to.

Keep answers short. This is a conversation, not a report.`

// Manager owns one chat session per project.
type Manager struct {
	db       *store.DB
	registry *adapter.Registry
	bus      *event.Bus
	log      *slog.Logger
	stateDir string

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	cer    *cerebrate.Cerebrate
	cancel context.CancelFunc
}

func NewManager(db *store.DB, reg *adapter.Registry, bus *event.Bus, log *slog.Logger, stateDir string) *Manager {
	return &Manager{
		db: db, registry: reg, bus: bus, log: log, stateDir: stateDir,
		sessions: map[string]*session{},
	}
}

// Ask sends a message and returns once the agent has it. The reply arrives as
// events, the same way every other agent's output does.
func (m *Manager) Ask(ctx context.Context, projectID, text string) error {
	if text == "" {
		return fmt.Errorf("nothing to ask")
	}

	// The question goes on the record before the answer, so a reload shows the
	// conversation rather than replies with nothing to reply to.
	m.bus.Publish(event.Event{
		Event:     adapter.Event{Kind: adapter.EventMessage, Text: text},
		ID:        store.NewID(),
		ProjectID: projectID,
		Role:      Operator,
	})

	s, err := m.ensure(ctx, projectID)
	if err != nil {
		return err
	}
	if err := s.cer.WaitReady(ctx); err != nil {
		return fmt.Errorf("chat agent did not start: %w", err)
	}
	return s.cer.Submit(text)
}

// ensure starts the session for a project if it is not already running.
//
// One long-lived process per project rather than one per message: a follow-up
// question that has forgotten the previous answer is not a conversation, and
// re-spawning would also re-read the repository every time.
func (m *Manager) ensure(ctx context.Context, projectID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[projectID]; ok && s.cer.State() != cerebrate.StateFailed {
		return s, nil
	}

	project, err := m.db.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}

	// The harness and model come from the team, so chat matches the work rather
	// than being configured twice. The terminal role is the one that reviews
	// everything, and is the best-informed choice available.
	team, err := m.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var pick store.ResolvedRole
	for _, r := range team {
		if r.Enabled {
			pick = r
		}
	}
	if pick.Harness == "" {
		return nil, fmt.Errorf("this project has no enabled roles, so there is no harness to chat with")
	}

	ad, err := m.registry.Get(pick.Harness)
	if err != nil {
		return nil, err
	}

	// Its own worktree: a question must never be able to touch the operator's
	// checkout, and the base branch is the state worth answering about.
	hat := hatchery.New(project.Path)
	worktree, err := hat.EnsureWorktree(ctx, Role, project.BaseBranch)
	if err != nil {
		return nil, fmt.Errorf("preparing a worktree for chat: %w", err)
	}

	role := pick
	role.Name = Role
	role.Prompt = systemPrompt

	cer := cerebrate.New(cerebrate.Config{
		ProjectID: projectID,
		Role:      role,
		Adapter:   ad,
		Worktree:  worktree,
		// No socket and no token, on purpose. This agent has no business
		// claiming work, and the absence is the enforcement.
		Bus:          m.bus,
		Log:          m.log,
		SystemPrompt: systemPrompt,
	})

	// Not tied to the request that started it: the session outlives the message
	// so the next question reaches the same process.
	runCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := cer.Run(runCtx); err != nil && runCtx.Err() == nil {
			m.log.Error("chat session ended", "project", projectID, "err", err)
		}
	}()

	s := &session{cer: cer, cancel: cancel}
	m.sessions[projectID] = s
	return s, nil
}

// Stop ends a project's chat session, if one is running.
func (m *Manager) Stop(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[projectID]; ok {
		s.cancel()
		delete(m.sessions, projectID)
	}
}

// StopAll ends every session, for daemon shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.cancel()
		delete(m.sessions, id)
	}
}

// Running reports whether a project has a live chat session.
func (m *Manager) Running(projectID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[projectID]
	return ok && s.cer.State() != cerebrate.StateFailed
}
