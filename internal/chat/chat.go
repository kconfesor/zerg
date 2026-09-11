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

// ReviewChat is the conversation a question asked from a diff runs in.
//
// Derived rather than stored, and with no row in the chats table, so it is a
// working session and never a tab. One per project, because these questions
// take turns anyway.
//
// Exported so a caller outside this package can ask Busy the same question
// AskAndWait already answers with it -- requestGuide's own precheck used to
// pass a bare project id, which never matched this and so never fired. See
// docs/design/code-explorer.md decision 8.
func ReviewChat(projectID string) string { return "review-" + projectID }

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

// WorktreeName is the role name a conversation's checkout is filed under, for
// anything outside this package that has to say where an agent is working.
func WorktreeName(chatID string) string { return worktreeFor(chatID) }

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

` + store.NoSubagentsInstruction + `

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

	// closing is the conversations being torn down.
	//
	// End stops the session and removes the worktree, and a queue drain that
	// was already in flight would build both again a moment later: a directory
	// and a process belonging to a tab that is gone, with nothing left to find
	// or stop them. Delivery checks this under the same lock that sets it.
	closing map[string]bool

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

	// turns is the conversations whose chat session is mid-turn.
	//
	// One session answers both the chat screen and a question asked from a
	// review, and its output is a stream with nobody's name on it. A review
	// question that overlapped an ordinary chat message collected that
	// message's answer and recorded it on the thread, under the agent's name,
	// as though it were about the code. There is nothing in an event to tell
	// them apart, so they take turns instead.
	//
	// Each claim carries a token rather than just marking the chatID taken.
	// Stop ends a session out from under whatever was waiting on it, and a
	// waiter whose own caller gave up keeps a reservation open in the
	// background until the agent actually finishes (see drainAbandonedAsk) --
	// both leave a claim outliving the code that took it, and a bare bool
	// cannot tell that claim apart from whatever claims the same chatID next.
	turns   map[string]turnClaim
	turnSeq uint64
}

// turnClaim is one admitted turn on one conversation.
//
// cancel wakes whoever is waiting on it -- AskAndWait's own select loop --
// the moment this specific claim ends for a reason that has nothing to do
// with the agent answering: Stop, or a later claim superseding this one.
// Nil for a claim nothing is synchronously waiting on (Ask's, and
// drainAbandonedAsk's own continuation of one).
type turnClaim struct {
	token  uint64
	cancel context.CancelFunc
}

// ErrClosed is returned when a conversation is being ended.
//
// Not a fault: it is a message that lost a race with the person closing the
// tab it was addressed to, and the right answer is to stop rather than to
// rebuild what was just removed.
var ErrClosed = errors.New("this conversation has been closed")

// ErrBusy is returned when the project's chat session is already answering.
//
// A distinct error because it is not a fault: it is the operator's to wait
// out, and the API turns it into a 409 rather than a 500.
var ErrBusy = errors.New("the agent is in the middle of an answer; ask again when it finishes")

// beginTurn claims a conversation for one question, or reports it taken.
//
// The context returned is derived from base and is what the claim's own
// waiter must select on: it is cancelled the moment this specific claim
// ends for a reason that has nothing to do with the agent answering --
// Stop, or a later claim superseding it -- and only that, never an
// unrelated chatID's turn ending. base is the caller's own ctx for a
// synchronous waiter (AskAndWait), so a caller timeout is inherited
// automatically, or context.Background() for a claim nothing is
// synchronously waiting on (Ask's), whose only source of early cancellation
// is Stop.
func (m *Manager) beginTurn(base context.Context, chatID string) (context.Context, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.turns[chatID]; busy {
		return nil, 0, false
	}
	if m.turns == nil {
		m.turns = map[string]turnClaim{}
	}
	m.turnSeq++
	token := m.turnSeq
	turnCtx, cancel := context.WithCancel(base)
	m.turns[chatID] = turnClaim{token: token, cancel: cancel}
	return turnCtx, token, true
}

// endTurn releases a conversation, but only the claim named by token -- a
// call whose own claim was already superseded (by Stop, or by a later
// beginTurn on the same chatID once this one gave it up) must not clear
// whatever claimed the conversation next.
func (m *Manager) endTurn(chatID string, token uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if claim, ok := m.turns[chatID]; ok && claim.token == token {
		delete(m.turns, chatID)
	}
}

// Busy reports whether a conversation is mid-answer. Advisory: the caller that
// wants it still has to win beginTurn.
func (m *Manager) Busy(chatID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, busy := m.turns[chatID]
	return busy
}

type session struct {
	cer    *cerebrate.Cerebrate
	cancel context.CancelFunc

	// projectID is which project this conversation belongs to, for the things
	// the manager says on its behalf.
	projectID string

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
	// Two files picked in one go are routinely called the same thing --
	// screenshot.png and screenshot.png, from two folders -- and copying both
	// to one name left the prompt naming that path twice, with the second file
	// answering for both. Names are made unique within the message.
	taken := map[string]bool{}
	for i := range files {
		name := uniqueName(files[i].Name, taken)
		taken[name] = true
		if err := copyFile(files[i].Source, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("attaching %s: %w", files[i].Name, err)
		}
		files[i].Path = filepath.Join(attachDir, name)
	}
	return nil
}

// uniqueName keeps a name unless this message already used it, in which case it
// numbers it the way a file manager does: screenshot.png, screenshot-2.png.
//
// Only within one message. The same name attached again later is the ordinary
// case -- a screenshot retaken after a fix -- and the newer one replacing the
// older is what somebody talking about "the screenshot" means.
func uniqueName(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if !taken[candidate] {
			return candidate
		}
	}
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
		turns:    map[string]turnClaim{},
		pending:  map[string][]Message{},
		closing:  map[string]bool{},
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
	// context.Background(), not ctx: nothing here waits synchronously on the
	// claim, so its only source of early cancellation is Stop, never this
	// request's own lifetime.
	turnCtx, token, ok := m.beginTurn(context.Background(), chatID)
	if !ok {
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
		m.endTurn(chatID, token)
		return err
	}
	// The chat screen does not wait for its own answer, so something has to:
	// the conversation is this question's until the agent stops talking.
	go m.releaseAtTurnEnd(projectID, chatID, token, turnCtx, events, cancel)
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
	m.mu.Unlock()
	if !ok {
		return nil
	}
	// The stop first, the queue after. Dropping what was typed before asking
	// meant a harness that cannot be interrupted -- pi says so rather than
	// pretending -- answered "cannot stop mid-answer" with the follow-ups
	// already gone: they were on the transcript, so they came back on reload
	// having never reached the agent.
	if err := s.cer.Interrupt(); err != nil {
		return err
	}
	m.mu.Lock()
	dropped := m.pending[chatID]
	delete(m.pending, chatID)
	m.mu.Unlock()

	// Said, not silently discarded. A message that was typed, recorded, and
	// then dropped by a stop is a hole in the conversation unless the
	// conversation says so.
	for range dropped {
		m.bus.Publish(event.Event{
			Event: adapter.Event{
				Kind: adapter.EventMessage,
				Text: "(not sent: the answer it was waiting behind was stopped)",
			},
			ID:        store.NewID(),
			ProjectID: s.projectID,
			Role:      Role,
			ChatID:    chatID,
		})
	}
	return nil
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
func (m *Manager) releaseAtTurnEnd(
	projectID, chatID string, token uint64, turnCtx context.Context,
	events <-chan event.Event, cancel func(),
) {
	defer m.drainOrRelease(projectID, chatID, token, turnCtx)
	defer cancel()

	backstop := time.After(turnBackstop)
	for {
		select {
		case <-turnCtx.Done():
			// Stop cancelled this specific claim -- the session it belonged
			// to is already gone, and drainOrRelease's own token check is
			// what keeps this from touching whatever claimed the chatID
			// next.
			return
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
			case adapter.EventError:
				return
			case adapter.EventDone:
				// Not turn_end: pi's own turn_end fires once per model call,
				// including the ones that only decided to call a tool, and
				// releasing there would let the next queued question start
				// into the middle of this answer -- the crossed wire this
				// exists to prevent. EventDone is the harness saying nothing
				// further is coming until something new is submitted. See
				// EventDone's own doc.
				return
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
func (m *Manager) drainOrRelease(projectID, chatID string, token uint64, turnCtx context.Context) {
	m.mu.Lock()
	// The claim named by token, not merely whatever is at chatID now: Stop
	// can end this exact claim and let a fresh one take the chatID before
	// this deferred call runs, and whatever is in m.pending by then belongs
	// to that fresh claim, not to this one to deliver or drop.
	if claim, ok := m.turns[chatID]; !ok || claim.token != token {
		m.mu.Unlock()
		return
	}
	queued := m.pending[chatID]
	if len(queued) == 0 {
		delete(m.turns, chatID)
		m.mu.Unlock()
		return
	}
	next := queued[0]
	m.pending[chatID] = queued[1:]
	m.mu.Unlock()

	// Its own subscription: the turn that just ended owns none any more.
	// turnCtx keeps coming from the original beginTurn, not a new one --
	// the claim it is cancelled by has not changed just because the message
	// it is answering has.
	events, cancel := m.bus.Subscribe(256)
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	if err := m.deliver(ctx, projectID, chatID, next); err != nil {
		m.log.Warn("could not send a queued chat message", "chat", chatID, "err", err)
		cancel()
		m.endTurn(chatID, token)
		return
	}
	go m.releaseAtTurnEnd(projectID, chatID, token, turnCtx, events, cancel)
}

// ensure starts the session for a project if it is not already running.
//
// One long-lived process per project rather than one per message: a follow-up
// question that has forgotten the previous answer is not a conversation, and
// re-spawning would also re-read the repository every time.
func (m *Manager) ensure(ctx context.Context, projectID, chatID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A conversation being closed does not get a new session. Without this a
	// drain already in flight rebuilds the worktree and the process just after
	// End removed them, and nothing is left holding either.
	if m.closing[chatID] {
		return nil, ErrClosed
	}
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

	s := &session{cer: cer, cancel: cancel, worktree: worktree, projectID: projectID}
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
	// Cancelled, not just cleared: the session this claim belonged to no
	// longer exists, and anything still waiting on it -- AskAndWait's own
	// select loop, or releaseAtTurnEnd's -- needs to be told so directly
	// rather than left blocked on events that are never coming, or worse,
	// left running until its own unrelated timeout while a fresh claim on
	// this same chatID is already answering a different question.
	if claim, ok := m.turns[chatID]; ok {
		claim.cancel()
	}
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
	m.Stop(ReviewChat(projectID))
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
	// Marked before anything is removed, so a drain that is mid-flight is
	// refused rather than rebuilding what this is about to delete.
	m.mu.Lock()
	m.closing[chatID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.closing, chatID)
		m.mu.Unlock()
	}()

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
// the agent's own messages until the whole answer is in, and hands back what
// it said.
//
// One question at a time per conversation, not per daemon. Two overlapping
// asks on the *same* chatID would each collect the other's sentences, since
// the bus carries the session's output and not a reply addressed to a caller
// -- beginTurn below already refuses a second one. A question asked in one
// project used to wait out an unrelated one already answering in a different
// project first, behind a single lock covering every conversation in the
// daemon; the event loop below already filters on ev.ChatID, so two different
// conversations were never actually at risk of collecting each other's
// sentences, only serialised for no reason. See
// docs/design/code-explorer.md decision 8.
//
// Waits for EventDone, not EventTurnEnd. This used to return on the first
// turn_end that had collected any message at all, which was the whole
// answer for claude (its turn_end already means that) but not for pi: caught
// live against a real run, pi's own turn_end fires once per model call, and
// a call that only decides to read a file still carries a stopReason of
// "toolUse", not "stop" -- there was no way to tell "the model said
// something" apart from "the model is done" from turn_end alone. EventDone
// is that distinction, made once in the adapter rather than guessed at here.
//
// The claim outlives this call on two different exits, and each hands it to
// something else rather than dropping it: Stop cancels turnCtx directly, in
// which case the session this call was waiting on no longer exists and there
// is nothing further to collect; ctx (the caller's own timeout) expiring
// while turnCtx has not means the agent underneath may still be mid-turn, so
// the claim is handed to drainAbandonedAsk rather than released out from
// under a session that is still about to answer -- releasing it here would
// let a fresh question start and collect whatever this one was still
// waiting for. Checked directly: without this, a caller's own askTimeout
// firing freed the conversation immediately while the agent kept running,
// and the next question in was the one that got yesterday's answer.
func (m *Manager) AskAndWait(ctx context.Context, projectID, question string) (string, error) {
	if question == "" {
		return "", fmt.Errorf("nothing to ask")
	}

	// A conversation of its own, with no row in the tab list. A question asked
	// from inside a diff is not one of the person's threads: its answer belongs
	// on the review it was asked from, and putting it in a tab would mean their
	// conversations filling with questions they did not type. The events are
	// still tagged, so they never surface in a tab either.
	chatID := ReviewChat(projectID)

	// Claims this conversation for this question, or refuses immediately: no
	// waiting on a lock first to find out. Everything this collects has to
	// belong to this question. Based on ctx: a caller timeout is inherited by
	// turnCtx automatically, which is what lets the two exits below be told
	// apart by ctx.Err() alone.
	turnCtx, token, ok := m.beginTurn(ctx, chatID)
	if !ok {
		return "", ErrBusy
	}

	// Subscribed before the question is sent: a fast agent can answer before a
	// subscription taken afterwards exists, and the answer would be lost to a
	// caller that is still waiting for it.
	events, cancel := m.bus.Subscribe(256)

	if err := m.submit(ctx, projectID, chatID, Message{Text: question}); err != nil {
		cancel()
		m.endTurn(chatID, token)
		return "", err
	}

	var said []string
	for {
		select {
		case <-turnCtx.Done():
			if ctx.Err() != nil {
				// The caller gave up, not the conversation. The agent may
				// still be mid-turn and about to emit the real answer, or
				// nothing at all if it is not -- either way, something has
				// to keep the reservation until that is known, or a fresh
				// question here would race whatever this one is still about
				// to collect. Handed the live subscription rather than
				// resubscribing fresh: a second or two between the two would
				// be a window in which the real answer arrives and is seen
				// by nobody, holding the reservation for the full backstop
				// over an answer that in fact landed right away.
				go m.drainAbandonedAsk(projectID, chatID, token, events, cancel)
				// Whatever it managed to say is better than nothing: a long
				// answer cut short still tells the reader something.
				return strings.TrimSpace(strings.Join(said, "\n\n")), ctx.Err()
			}
			// Stop cancelled this claim directly: the session is gone, and
			// there is nothing left to wait for or to release -- Stop
			// already did that.
			cancel()
			return strings.TrimSpace(strings.Join(said, "\n\n")), turnCtx.Err()
		case ev, ok := <-events:
			if !ok {
				m.endTurn(chatID, token)
				cancel()
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
			case adapter.EventDone:
				m.endTurn(chatID, token)
				cancel()
				return strings.TrimSpace(strings.Join(said, "\n\n")), nil
			case adapter.EventError:
				m.endTurn(chatID, token)
				cancel()
				if len(said) == 0 {
					return "", fmt.Errorf("the agent could not answer: %s", ev.Text)
				}
				return strings.TrimSpace(strings.Join(said, "\n\n")), nil
			}
		}
	}
}

// drainAbandonedAsk keeps one AskAndWait call's reservation held after its
// own caller has stopped waiting, until the agent it was talking to actually
// finishes -- so a fresh ask on the same conversation is correctly told busy
// rather than started early and left to collect whatever this one is still
// about to say. Same backstop as releaseAtTurnEnd, and for the same reason: a
// harness that never emits another event must not hold a reservation for the
// life of the daemon.
//
// Takes over AskAndWait's own subscription rather than opening a new one:
// AskAndWait has already stopped reading events by the time this goroutine
// starts, so there is exactly one reader on the channel at any moment, never
// two racing for the same event.
func (m *Manager) drainAbandonedAsk(
	projectID, chatID string, token uint64, events <-chan event.Event, cancel func(),
) {
	defer cancel()
	defer m.endTurn(chatID, token)

	backstop := time.After(turnBackstop)
	for {
		select {
		case <-backstop:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.ProjectID != projectID || ev.Role != Role || ev.ChatID != chatID {
				continue
			}
			if ev.Kind == adapter.EventDone || ev.Kind == adapter.EventError {
				return
			}
		}
	}
}
