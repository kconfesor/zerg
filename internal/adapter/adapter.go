// Package adapter defines the contract every agent harness must satisfy.
//
// swarm-forge hardcoded its four backends into a validation set and a case
// expression, so adding one meant editing the launcher. Here a harness is a
// registered implementation of Adapter, and the orchestrator never names a
// specific CLI.
package adapter

import (
	"context"
	"os/exec"
	"time"
)

// Adapter drives one agent harness (claude, pi, ...).
type Adapter interface {
	// Name is the identifier used in brood config: harness = "pi".
	Name() string

	// Checks returns harness-specific preflight checks, run before any spawn.
	// Generic checks (binary present, workspace trusted) are supplied by the
	// runner; these cover what only the adapter knows — config file shape,
	// credential probing, model catalog.
	Checks() []Check

	// ListModels asks the harness what it can actually run, so the role editor
	// renders a picker instead of a text box. Typing model ids by hand is how
	// you get "Model metadata for 'gpt-5.6-sol' not found" followed by twenty
	// minutes of an agent that looks alive while every turn returns HTTP 400.
	//
	// A catalog may lag reality — gpt-5.6-sol is absent from pi's list and runs
	// fine — so the UI still accepts free text. An unlisted model warns; it
	// does not block.
	ListModels(ctx context.Context) ([]Model, error)

	// Command builds the process to run. It must not mutate global state:
	// anything the harness would write to a shared config directory belongs
	// under Spec.ConfigDir.
	Command(ctx context.Context, spec Spec) (*exec.Cmd, error)

	// Parse converts one line of the harness's structured output into a typed
	// Event. Returning (nil, nil) means "no event, not an error" — harnesses
	// emit banners and warnings that carry no semantics.
	Parse(line []byte) (*Event, error)

	Capabilities() Caps
}

// Model is one entry from a harness's own catalog.
type Model struct {
	ID       string // harness-native id, e.g. "opus" or "openai-codex/gpt-5.6-sol"
	Label    string // display name for the picker
	Provider string // grouping in the UI; empty when the harness has one provider
	Context  int    // context window in tokens, 0 when unknown
}

// Spec is everything needed to launch one agent. Every field originates in the
// database — there is no config file to read and no snapshot in a worktree to
// go stale.
type Spec struct {
	Role     string
	Worktree string // agent cwd; a git worktree owned by this role
	Model    string
	ExtraArgs []string

	// SystemFile is composed fresh at every spawn from the shared instructions
	// plus this role's prompt, both read from the database. The predecessor
	// copied prompt files into each worktree at creation time, so edits made
	// afterward reached nobody: a config set to Rust produced a Clojure
	// implementation across six agents, silently.
	SystemFile string

	// ConfigDir is a private, per-role harness config directory. Two agents
	// launching concurrently must never read-modify-write one shared global
	// config — that race triplicated ~/.codex/config.toml and broke codex
	// machine-wide, for unrelated projects too.
	ConfigDir string

	// Socket and Token reach the agent as env vars. The agent authenticates to
	// the overmind with Token; there is no --from flag to forge.
	Socket string
	Token  string
}

// Caps declares what a harness can actually do, so the orchestrator can degrade
// deliberately rather than discovering a gap at runtime.
type Caps struct {
	// StructuredOutput is required for the primary path: the harness emits
	// machine-readable events rather than painting a screen.
	StructuredOutput bool

	// StructuredInput means the harness accepts streaming structured input
	// while running headless (claude --input-format stream-json, pi --mode
	// rpc). With it, chat and clarification answers reach a live agent as
	// messages. Without it, a role can receive work only between turns.
	StructuredInput bool

	// InteractiveTUI means the harness has a real terminal UI that a human can
	// drive, used for takeover (see ARCHITECTURE.md §10.1). Takeover is a mode
	// switch, not a parallel view: a process is either emitting structured
	// events or painting a screen, never both.
	InteractiveTUI bool

	SystemPromptFile bool // accepts a system prompt as a file rather than an argv blob
	ModelFlag        bool // model selectable per invocation
	ResumeSession    bool // can resume after a crash without losing context
}

// Check is one preflight probe. Every incident worth a postmortem in the
// predecessor system — a stale CLI rejecting its model, a corrupted config, an
// unanswered trust dialog, a broken plugin tree — was a Check that did not exist.
type Check struct {
	Name string
	Run  func(ctx context.Context, spec Spec) Result
}

type Result struct {
	OK bool

	// Warn reports a concern that must not block the spawn — an unlisted model
	// id, for instance, since a harness catalog can lag a model that works.
	Warn bool

	// Reason states what is wrong, Remedy states the command that fixes it.
	// A blocked role renders in the cockpit with both. It never renders as an
	// idle pane that happens to be doing nothing.
	//
	// Credentials are detect-only: zerg reports "pi: no credentials for
	// provider 'openai'" with the remedy, and never runs a login flow or
	// touches a harness auth file.
	Reason string
	Remedy string
}

// EventKind is the vocabulary the cockpit renders. Adapters map their harness's
// native output onto it; the UI knows nothing about any specific harness.
type EventKind string

const (
	EventReady     EventKind = "ready"      // agent booted and is accepting work
	EventThinking  EventKind = "thinking"   // reasoning, no tool call yet
	EventToolCall  EventKind = "tool_call"  // invoked a tool
	EventToolDone  EventKind = "tool_done"  // tool returned
	EventMessage   EventKind = "message"    // assistant prose
	EventUsage     EventKind = "usage"      // tokens and cost for a turn
	EventTurnEnd   EventKind = "turn_end"   // finished a turn, likely idle now
	EventError     EventKind = "error"      // harness-level failure, carries Fatal
)

type Event struct {
	Kind EventKind
	Role string
	At   time.Time

	Text string // prose or tool name, depending on Kind
	Tool string
	Args map[string]any

	TokensIn  int
	TokensOut int
	CostUSD   float64

	// Fatal marks an error the agent cannot recover from, so the cerebrate
	// stops instead of leaving a process that looks alive and answers nothing.
	// The predecessor's codex agents sat at a prompt for 20 minutes returning
	// HTTP 400 on every turn, indistinguishable from working.
	Fatal bool
}
