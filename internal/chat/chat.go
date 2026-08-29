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
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/cerebrate"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// Role is the name chat events carry.
//
// A name rather than a flag on the event, so the existing role filter, colour
// assignment and replay all work on it with no special case. Which conversation
// an event belongs to is a column beside it: encoding an id in the role would
// make every one of those a string match.
const Role = "chat"

// reviewChat is the conversation a question asked from a diff runs in.
//
// Derived rather than stored, and with no row in the chats table, so it is a
// working session and never a tab. One per project, because these questions
// take turns anyway.
func reviewChat(projectID string) string { return "review-" + projectID }

// worktreeFor is where one conversation's agent runs.
//
// A checkout per conversation, named after it. They are separate agents holding
// separate threads, and sharing a directory would mean one tab's attachments
// appearing in another's listing and two agents writing to the same place when
// both are answering. The id is the name, so ending a tab removes exactly its
// own.
func worktreeFor(chatID string) string {
	return Role + "-" + chatID
}

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

	// blobs holds what people attach, so ending a chat can take the files with
	// it. Optional: a build without a store simply has nothing to remove.
	blobs *artifact.Store

	// Everything below is keyed by conversation, not by project: a person can
	// have several open, and each is its own thread with its own agent.
	mu       sync.Mutex
	sessions map[string]*session

	// asking serialises AskAndWait. The bus carries a session's output rather
	// than a reply addressed to a caller, so two overlapping questions would
	// each collect the other's sentences.
	asking sync.Mutex

	// pending is what has been typed while the agent was still answering,
	// per project, in the order it was typed.
	//
	// Refusing it was the honest thing to do when there was nowhere to put it,
	// and it made the screen behave unlike every other chat a person has used:
	// a thought had while reading an answer had to be held in the head until
	// the answer finished. The session still takes one turn at a time -- that
	// is the harness's constraint, not this one's -- so the queue is drained
	// when a turn ends.
	pending map[string][]Message

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

// beginTurn claims a conversation for one question, or reports it taken.
func (m *Manager) beginTurn(chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns[chatID] {
		return false
	}
	if m.turns == nil {
		m.turns = map[string]bool{}
	}
	m.turns[chatID] = true
	return true
}

func (m *Manager) endTurn(chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.turns, chatID)
}

// Busy reports whether a conversation is mid-answer. Advisory: the caller that
// wants it still has to win beginTurn.
func (m *Manager) Busy(chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns[chatID]
}

type session struct {
	cer    *cerebrate.Cerebrate
	cancel context.CancelFunc

	// worktree is where this session's agent is running, which is where an
	// attachment has to be for it to read one.
	worktree string
}

// attachDir is where uploads land inside the chat worktree.
//
// Named rather than hidden, and inside the worktree rather than beside it: the
// agent is told a path and may well list the directory to see what else came
// with it, and a dot-directory is one it would have to be told about twice.
const attachDir = "attachments"

// materialise copies uploads into the worktree and fills in where they landed.
//
// Copied rather than linked or read from the store: the agent's tools are
// pointed at its own worktree, and a path outside it is both awkward to explain
// and a route out of the only directory this agent is supposed to touch. The
// bytes stay in the store as well, which is what keeps the picture in the
// conversation after the worktree is removed.
func (m *Manager) materialise(worktree string, files []Attachment) error {
	if len(files) == 0 {
		return nil
	}
	dir := filepath.Join(worktree, attachDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("making room for attachments: %w", err)
	}
	for i := range files {
		dst := filepath.Join(dir, files[i].Name)
		if err := copyFile(files[i].Source, dst); err != nil {
			return fmt.Errorf("attaching %s: %w", files[i].Name, err)
		}
		files[i].Path = filepath.Join(attachDir, files[i].Name)
	}
	return nil
}

// copyFile writes src to dst, replacing whatever was there.
//
// The same name attached twice in one conversation is the ordinary case --
// screenshot.png, then screenshot.png again -- and the newer one is the one
// being talked about.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Message is one thing a person said, and whatever they attached to it.
type Message struct {
	Text string

	// Files are paths inside the chat worktree, already written. Named in the
	// prompt rather than sent as content: the agent has a filesystem and the
	// tools to read it, and a path works the same for a screenshot, a log and
	// a CSV.
	Files []Attachment
}

// Attachment is one uploaded file, as the agent will see it.
type Attachment struct {
	// Name is what the person called it, for the transcript and for the copy
	// the agent reads.
	Name string

	// Source is where the bytes are now: the blob store, under their digest.
	Source string

	// Path is where the agent finds them, filled in when the copy is made.
	// Inside the chat worktree, because an agent asked to look at something
	// should not have to be told about a directory belonging to the daemon.
	Path string

	// ArtifactID is the row holding the bytes, so the conversation can still
	// show the picture after the worktree is gone.
	ArtifactID string
}

// WithBlobs gives the manager the store holding attachments.
func (m *Manager) WithBlobs(blobs *artifact.Store) *Manager {
	m.blobs = blobs
	return m
}

func NewManager(db *store.DB, reg *adapter.Registry, bus *event.Bus, log *slog.Logger, stateDir string) *Manager {
	return &Manager{
		db: db, registry: reg, bus: bus, log: log, stateDir: stateDir,
		sessions: map[string]*session{},
		turns:    map[string]bool{},
		pending:  map[string][]Message{},
	}
}

// Ask sends a message and returns once the agent has it. The reply arrives as
// events, the same way every other agent's output does.
func (m *Manager) Ask(ctx context.Context, projectID, chatID string, msg Message) error {
	if strings.TrimSpace(msg.Text) == "" && len(msg.Files) == 0 {
		return fmt.Errorf("nothing to ask")
	}
	if chatID == "" {
		return fmt.Errorf("a message belongs to a conversation")
	}
	// The tab moves to the front, and takes its name from the first thing said
	// in it when nobody has named it: "Chat 3" says nothing about which one it
	// is, and the opening sentence nearly always does.
	if err := m.db.TouchChat(ctx, chatID); err != nil {
		m.log.Warn("could not mark a conversation used", "chat", chatID, "err", err)
	}
	if c, err := m.db.GetChat(ctx, projectID, chatID); err == nil && c.Title == "" {
		if err := m.db.NameChat(ctx, chatID, firstLine(msg.Text)); err != nil {
			m.log.Warn("could not name a conversation", "chat", chatID, "err", err)
		}
	}
	// Typed while the agent was still writing. It goes on the record now, in
	// the order it was typed, and is sent when the turn in flight ends: the
	// alternative was refusing it, which made the screen behave unlike every
	// chat a person has used.
	if !m.beginTurn(chatID) {
		m.record(projectID, chatID, msg)
		m.mu.Lock()
		m.pending[chatID] = append(m.pending[chatID], msg)
		m.mu.Unlock()
		return nil
	}
	// Subscribed before the question is sent: an answer that arrives before
	// the subscription exists would leave the session marked busy until the
	// backstop, and every question in between refused.
	events, cancel := m.bus.Subscribe(256)
	if err := m.submit(ctx, projectID, chatID, msg); err != nil {
		cancel()
		m.endTurn(chatID)
		return err
	}
	// The chat screen does not wait for its own answer, so something has to:
	// the conversation is this question's until the agent stops talking.
	go m.releaseAtTurnEnd(projectID, chatID, events, cancel)
	return nil
}

// firstLine is a title taken from what somebody opened with.
func firstLine(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len([]rune(line)) > 60 {
		line = strings.TrimSpace(string([]rune(line)[:60]))
	}
	return line
}

// Interrupt stops the answer being written, without ending the conversation.
//
// The session stays up, because it is where the conversation lives: stopping a
// reply by killing the process answers "not that, I meant something else" by
// forgetting everything said so far. Whatever was queued behind this turn is
// dropped with it -- a follow-up typed against an answer you have just stopped
// is about a reply that no longer exists.
func (m *Manager) Interrupt(chatID string) error {
	m.mu.Lock()
	s, ok := m.sessions[chatID]
	delete(m.pending, chatID)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return s.cer.Interrupt()
}

// Queued is how many messages are waiting behind the answer in flight.
func (m *Manager) Queued(chatID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending[chatID])
}

// record puts what the person said on the transcript.
//
// Before the answer, and before the message is even sent when it is queued: a
// reload has to show the conversation, and a question that appears only once
// its reply arrives reads as a screen that lost what you typed.
func (m *Manager) record(projectID, chatID string, msg Message) {
	m.bus.Publish(event.Event{
		Event:     adapter.Event{Kind: adapter.EventMessage, Text: msg.Text, Args: attachmentArgs(msg.Files)},
		ID:        store.NewID(),
		ProjectID: projectID,
		Role:      Operator,
		ChatID:    chatID,
	})
}

// attachmentArgs carries the files on the event, so the conversation can show
// them again after a reload. Nil when there are none, so an ordinary message
// stores no payload at all.
func attachmentArgs(files []Attachment) map[string]any {
	if len(files) == 0 {
		return nil
	}
	out := make([]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{"name": f.Name, "artifactId": f.ArtifactID})
	}
	return map[string]any{"attachments": out}
}

// submit records the question and hands it to the session.
func (m *Manager) submit(ctx context.Context, projectID, chatID string, msg Message) error {
	// The question goes on the record before the answer, so a reload shows the
	// conversation rather than replies with nothing to reply to.
	m.record(projectID, chatID, msg)
	return m.deliver(ctx, projectID, chatID, msg)
}

// deliver hands a message to the session without recording it again, for the
// queued ones that went on the transcript when they were typed.
func (m *Manager) deliver(ctx context.Context, projectID, chatID string, msg Message) error {
	s, err := m.ensure(ctx, projectID, chatID)
	if err != nil {
		return err
	}
	if err := m.materialise(s.worktree, msg.Files); err != nil {
		return err
	}
	if err := s.cer.WaitReady(ctx); err != nil {
		return fmt.Errorf("chat agent did not start: %w", err)
	}
	return s.cer.Submit(prompt(msg))
}

// prompt is what the agent is actually sent: what was said, and where to find
// whatever came with it.
//
// The paths are named rather than the contents inlined. An agent has a
// filesystem and the tools to read it, so a path works the same for a
// screenshot, a log and a spreadsheet, and a large file does not have to
// survive being pasted into a prompt to be read.
func prompt(msg Message) string {
	text := strings.TrimSpace(msg.Text)
	if len(msg.Files) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	if len(msg.Files) == 1 {
		b.WriteString("Attached, in this worktree: ")
	} else {
		b.WriteString("Attached, in this worktree:\n")
	}
	for i, f := range msg.Files {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(f.Path)
	}
	return b.String()
}

// turnBackstop frees a session whose agent never said it had finished.
//
// A harness killed mid-turn emits nothing more, and without this the project
// would be marked busy for the life of the daemon and every later question
// refused. Long enough not to cut a real answer short.
const turnBackstop = 5 * time.Minute

// releaseAtTurnEnd frees the session when the agent stops talking, and sends
// whatever was typed while it was.
func (m *Manager) releaseAtTurnEnd(projectID, chatID string, events <-chan event.Event, cancel func()) {
	defer m.drainOrRelease(projectID, chatID)
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
			// This conversation's agent, not another tab's: several can be
			// answering at once, and a turn that ended over there says nothing
			// about the one being waited on here.
			if ev.ProjectID != projectID || ev.Role != Role || ev.ChatID != chatID {
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

// drainOrRelease sends the next queued message, or frees the session when
// there is none.
//
// The turn is handed straight to the next message rather than released and
// re-taken: releasing first would let a review's question in between two
// things a person typed in a row, and that question would then collect the
// answer to theirs.
func (m *Manager) drainOrRelease(projectID, chatID string) {
	m.mu.Lock()
	queued := m.pending[chatID]
	if len(queued) == 0 {
		m.mu.Unlock()
		m.endTurn(chatID)
		return
	}
	next := queued[0]
	m.pending[chatID] = queued[1:]
	m.mu.Unlock()

	// Its own context and its own subscription: the turn that just ended owns
	// neither any more.
	events, cancel := m.bus.Subscribe(256)
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	if err := m.deliver(ctx, projectID, chatID, next); err != nil {
		m.log.Warn("could not send a queued chat message", "chat", chatID, "err", err)
		cancel()
		m.endTurn(chatID)
		return
	}
	go m.releaseAtTurnEnd(projectID, chatID, events, cancel)
}

// ensure starts the session for a project if it is not already running.
//
// One long-lived process per project rather than one per message: a follow-up
// question that has forgotten the previous answer is not a conversation, and
// re-spawning would also re-read the repository every time.
func (m *Manager) ensure(ctx context.Context, projectID, chatID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[chatID]; ok && s.cer.State() != cerebrate.StateFailed {
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
	worktree, err := hat.EnsureWorktree(ctx, worktreeFor(chatID), project.BaseBranch)
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
		// The one session somebody watches being written. A pipeline role's
		// output is read afterwards, if at all, so it is not asked for there.
		Streaming: true,
		// Which conversation this is, stamped on everything it says: several
		// tabs can be answering at once.
		ChatID: chatID,
	})

	// Not tied to the request that started it: the session outlives the message
	// so the next question reaches the same process.
	runCtx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := cer.Run(runCtx); err != nil && runCtx.Err() == nil {
			m.log.Error("chat session ended", "project", projectID, "err", err)
		}
	}()

	s := &session{cer: cer, cancel: cancel, worktree: worktree}
	m.sessions[chatID] = s
	return s, nil
}

// Stop ends one conversation's session, if it is running.
func (m *Manager) Stop(chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[chatID]; ok {
		s.cancel()
		delete(m.sessions, chatID)
	}
	delete(m.pending, chatID)
	delete(m.turns, chatID)
}

// StopProject ends every conversation in a project.
//
// The agent behind them was chosen per project, so changing it has to reach all
// of them: a tab left running would keep answering as the model the person just
// stopped using.
func (m *Manager) StopProject(ctx context.Context, projectID string) {
	chats, err := m.db.ListChats(ctx, projectID)
	if err != nil {
		m.log.Warn("could not list conversations to stop", "project", projectID, "err", err)
		return
	}
	for _, c := range chats {
		m.Stop(c.ID)
	}
	m.Stop(reviewChat(projectID))
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
func (m *Manager) Running(chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[chatID]
	return ok && s.cer.State() != cerebrate.StateFailed
}

// End closes one conversation and removes everything it had.
//
// The worktree is the reason this exists at all: a conversation gets its own
// checkout so a question can never touch the operator's, and that checkout
// accumulates whatever the conversation did -- attachments, scratch files, a
// branch that has drifted from base. Stopping the session leaves all of it.
//
// The transcript and the files go too, which is the whole contract of a tab:
// closing one is a decision, and what it removes is everything that was only
// ever part of it.
func (m *Manager) End(ctx context.Context, projectID, chatID string) error {
	if _, err := m.db.GetChat(ctx, projectID, chatID); err != nil {
		return err
	}
	m.Stop(chatID)

	project, err := m.db.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := hatchery.New(project.Path).RemoveWorktree(ctx, worktreeFor(chatID)); err != nil {
		return fmt.Errorf("removing the conversation's worktree: %w", err)
	}

	orphans, err := m.db.DeleteChat(ctx, projectID, chatID)
	if err != nil {
		return fmt.Errorf("clearing the conversation: %w", err)
	}
	if m.blobs != nil {
		for _, digest := range orphans {
			if err := m.blobs.Remove(digest); err != nil {
				// The rows are gone, so the conversation is gone; a file left
				// on disk is waste, not a wrong answer, and nothing will look
				// for it again. Said rather than returned.
				m.log.Warn("could not remove an attachment's bytes",
					"chat", chatID, "digest", digest, "err", err)
			}
		}
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

	// A conversation of its own, with no row in the tab list. A question asked
	// from inside a diff is not one of the person's threads: its answer belongs
	// on the review it was asked from, and putting it in a tab would mean their
	// conversations filling with questions they did not type. The events are
	// still tagged, so they never surface in a tab either.
	chatID := reviewChat(projectID)

	// Waits for the session rather than talking over it, and holds it until
	// the answer is in hand: everything this collects has to belong to this
	// question.
	if !m.beginTurn(chatID) {
		return "", ErrBusy
	}
	defer m.endTurn(chatID)

	// Subscribed before the question is sent: a fast agent can answer before a
	// subscription taken afterwards exists, and the answer would be lost to a
	// caller that is still waiting for it.
	events, cancel := m.bus.Subscribe(256)
	defer cancel()

	if err := m.submit(ctx, projectID, chatID, Message{Text: question}); err != nil {
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
			if ev.ProjectID != projectID || ev.Role != Role || ev.ChatID != chatID {
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
