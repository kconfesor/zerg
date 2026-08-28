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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
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
actually in the tree rather than from what a project like this usually contains.
Read the files before describing them.

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

	// asking serialises AskAndWait. The bus carries a session's output rather
	// than a reply addressed to a caller, so two overlapping questions would
	// each collect the other's sentences.
	asking sync.Mutex

	// turns is the projects whose chat session is mid-turn.
	//
	// One session answers both the chat screen and a question asked from a
	// review, and its output is a stream with nobody's name on it. A review
	// question that overlapped an ordinary chat message collected that
	// message's answer and recorded it on the thread, under the agent's name,
	// as though it were about the code. There is nothing in an event to tell
	// them apart, so they take turns instead.
	turns map[string]bool
}

// ErrBusy is returned when the project's chat session is already answering.
//
// A distinct error because it is not a fault: it is the operator's to wait
// out, and the API turns it into a 409 rather than a 500.
var ErrBusy = errors.New("the agent is in the middle of an answer; ask again when it finishes")

// beginTurn claims the session for one question, or reports it taken.
func (m *Manager) beginTurn(projectID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns[projectID] {
		return false
	}
	if m.turns == nil {
		m.turns = map[string]bool{}
	}
	m.turns[projectID] = true
	return true
}

func (m *Manager) endTurn(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.turns, projectID)
}

// Busy reports whether the project's session is mid-answer. Advisory: the
// caller that wants the session still has to win beginTurn.
func (m *Manager) Busy(projectID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns[projectID]
}

type session struct {
	cer    *cerebrate.Cerebrate
	cancel context.CancelFunc
}

func NewManager(db *store.DB, reg *adapter.Registry, bus *event.Bus, log *slog.Logger, stateDir string) *Manager {
	return &Manager{
		db: db, registry: reg, bus: bus, log: log, stateDir: stateDir,
		sessions: map[string]*session{},
		turns:    map[string]bool{},
	}
}

// Ask sends a message and returns once the agent has it. The reply arrives as
// events, the same way every other agent's output does.
func (m *Manager) Ask(ctx context.Context, projectID, text string) error {
	if text == "" {
		return fmt.Errorf("nothing to ask")
	}
	if !m.beginTurn(projectID) {
		return ErrBusy
	}
	// Subscribed before the question is sent: an answer that arrives before
	// the subscription exists would leave the session marked busy until the
	// backstop, and every question in between refused.
	events, cancel := m.bus.Subscribe(256)
	if err := m.submit(ctx, projectID, text); err != nil {
		cancel()
		m.endTurn(projectID)
		return err
	}
	// The chat screen does not wait for its own answer, so something has to:
	// the session is this question's until the agent stops talking.
	go m.releaseAtTurnEnd(projectID, events, cancel)
	return nil
}

// submit records the question and hands it to the session.
func (m *Manager) submit(ctx context.Context, projectID, text string) error {
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

// turnBackstop frees a session whose agent never said it had finished.
//
// A harness killed mid-turn emits nothing more, and without this the project
// would be marked busy for the life of the daemon and every later question
// refused. Long enough not to cut a real answer short.
const turnBackstop = 5 * time.Minute

// releaseAtTurnEnd frees the session when the agent stops talking.
func (m *Manager) releaseAtTurnEnd(projectID string, events <-chan event.Event, cancel func()) {
	defer m.endTurn(projectID)
	defer cancel()

	said := false
	backstop := time.After(turnBackstop)
	for {
		select {
		case <-backstop:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.ProjectID != projectID || ev.Role != Role {
				continue
			}
			switch ev.Kind {
			case adapter.EventMessage:
				said = true
			case adapter.EventError:
				return
			case adapter.EventTurnEnd:
				// Not the first turn that ends: a session emits one before the
				// reply to this question arrives, and releasing there would let
				// the next question start into the middle of this answer, which
				// is the crossed wire this exists to prevent.
				if said {
					return
				}
			}
		}
	}
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

	// An explicit choice wins. Inheriting the terminal role is a good default —
	// it is the role that reads everything — and a poor rule, because it is
	// also usually the most expensive model on the team, and asking where a
	// function lives does not need it.
	if project.ChatHarness != "" {
		pick.Harness = project.ChatHarness
		pick.Model = project.ChatModel
	} else if project.ChatModel != "" {
		pick.Model = project.ChatModel
	}
	if pick.Harness == "" {
		return nil, fmt.Errorf(
			"no harness to chat with: this project has no enabled roles, and no chat harness is set")
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

// Reset ends a chat and removes what it left behind.
//
// The worktree is the reason this exists: chat gets its own checkout so a
// question can never touch the operator's, and that checkout accumulates
// whatever the conversation did — built artefacts, scratch files, a branch that
// has drifted from base. Stopping the session leaves all of it.
//
// The transcript goes too. It is a conversation rather than a record of work:
// nothing downstream refers to it, and a chat you have deliberately ended is
// not one whose history you wanted.
func (m *Manager) Reset(ctx context.Context, projectID string) error {
	m.Stop(projectID)

	project, err := m.db.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := hatchery.New(project.Path).RemoveWorktree(ctx, Role); err != nil {
		return fmt.Errorf("removing the chat worktree: %w", err)
	}
	if err := m.db.DeleteRoleEvents(ctx, projectID, Role); err != nil {
		return fmt.Errorf("clearing the conversation: %w", err)
	}
	return nil
}

// AskAndWait asks and returns the answer, rather than leaving it on the bus.
//
// Ask is right for the chat screen, where the reply streams into a conversation
// somebody is watching. A question asked from inside a diff has somewhere
// specific to land: the review thread it was asked on. So this one listens for
// the agent's own messages until it finishes its turn, and hands back what it
// said.
//
// One question at a time per project. Two overlapping asks would each collect
// the other's sentences, since the bus carries the session's output and not a
// reply addressed to a caller.
func (m *Manager) AskAndWait(ctx context.Context, projectID, question string) (string, error) {
	if question == "" {
		return "", fmt.Errorf("nothing to ask")
	}
	m.asking.Lock()
	defer m.asking.Unlock()

	// Waits for the session rather than talking over it, and holds it until
	// the answer is in hand: everything this collects has to belong to this
	// question.
	if !m.beginTurn(projectID) {
		return "", ErrBusy
	}
	defer m.endTurn(projectID)

	// Subscribed before the question is sent: a fast agent can answer before a
	// subscription taken afterwards exists, and the answer would be lost to a
	// caller that is still waiting for it.
	events, cancel := m.bus.Subscribe(256)
	defer cancel()

	if err := m.submit(ctx, projectID, question); err != nil {
		return "", err
	}

	var said []string
	for {
		select {
		case <-ctx.Done():
			// Whatever it managed to say is better than nothing: a long answer
			// cut short still tells the reader something.
			return strings.TrimSpace(strings.Join(said, "\n\n")), ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return strings.TrimSpace(strings.Join(said, "\n\n")), nil
			}
			if ev.ProjectID != projectID || ev.Role != Role {
				continue
			}
			switch ev.Kind {
			case adapter.EventMessage:
				if text := strings.TrimSpace(ev.Text); text != "" {
					said = append(said, text)
				}
			case adapter.EventTurnEnd:
				// Not the first turn that ends: the one that carried the
				// answer. A session emits a turn_end before this question's
				// reply arrives, and returning there gave the thread an empty
				// comment while the agent went on to answer perfectly well two
				// seconds later.
				if len(said) == 0 {
					continue
				}
				return strings.TrimSpace(strings.Join(said, "\n\n")), nil
			case adapter.EventError:
				if len(said) == 0 {
					return "", fmt.Errorf("the agent could not answer: %s", ev.Text)
				}
				return strings.TrimSpace(strings.Join(said, "\n\n")), nil
			}
		}
	}
}
