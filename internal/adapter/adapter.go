// Package adapter defines the contract every agent harness must satisfy.
//
// swarm-forge hardcoded its four backends into a validation set and a case
// expression, so adding one meant editing the launcher. Here a harness is a
// registered implementation of Adapter, and the orchestrator never names a
// specific CLI.
package adapter

import (
	"context"
	"os"
	"os/exec"
	"strings"
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

	// Parse converts one line of the harness's structured output into typed
	// events. One line can carry several — a single assistant message may hold
	// prose and two tool calls — so this returns a slice. An empty result means
	// "nothing meaningful", which is common: harnesses emit banners, hook
	// chatter and rate-limit notices that carry no semantics.
	Parse(line []byte) ([]Event, error)

	// EncodeTurn renders one turn as the bytes this harness expects on stdin.
	//
	// The two supported harnesses disagree completely — claude wants an SDK
	// user message, pi wants {"type":"prompt","message":...} — so this cannot
	// live in the supervisor. Both shapes were verified by driving a live
	// process over a pipe, not inferred.
	EncodeTurn(text string) ([]byte, error)

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
	Role      string
	Worktree  string // agent cwd; a git worktree owned by this role
	Model     string
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

	// BinDir holds the zerg executable and is prepended to the agent's PATH.
	//
	// Agents are instructed to run `zerg next`, `zerg done` and `zerg send`.
	// Inheriting the daemon's PATH is not enough: a daemon started by absolute
	// path, or installed anywhere not already on PATH, leaves those commands
	// unresolvable — and the failure is quiet, because the agent simply cannot
	// do what it was told and has no way to say why.
	BinDir string
}

// AgentEnv builds the environment for a spawned agent: the current environment
// with BinDir prepended to PATH, plus the identity the agent authenticates with.
func AgentEnv(spec Spec, extra ...string) []string {
	path := os.Getenv("PATH")
	if spec.BinDir != "" {
		path = spec.BinDir + string(os.PathListSeparator) + path
	}

	out := make([]string, 0, len(os.Environ())+4+len(extra))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"PATH="+path,
		"ZERG_SOCKET="+spec.Socket,
		"ZERG_TOKEN="+spec.Token,
		"ZERG_ROLE="+spec.Role,
	)
	return append(out, extra...)
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

	// PrivateConfigDir says whether this harness can run with its config
	// relocated per role.
	//
	// Isolation exists because two agents racing a read-modify-write of one
	// shared global config produced a file holding three concatenated copies
	// of itself. But it is not universally safe: claude keeps credentials in
	// the OS keychain and a relocated config directory breaks the lookup, so
	// an isolated agent launches unauthenticated. Losing auth entirely is
	// strictly worse than a rare race, so each adapter decides for itself.
	PrivateConfigDir bool

	SystemPromptFile bool // accepts a system prompt as a file rather than an argv blob
	ModelFlag        bool // model selectable per invocation
	ResumeSession    bool // can resume after a crash without losing context
}

// Ctx is context.Context, aliased so a Check reads on one line.
type Ctx = context.Context

// Check is one preflight probe. Every incident worth a postmortem in the
// predecessor system — a stale CLI rejecting its model, a corrupted config, an
// unanswered trust dialog, a broken plugin tree — was a Check that did not exist.
type Check struct {
	Name string
	Run  func(ctx Ctx, spec Spec) Result
}

type Result struct {
	OK bool

	// Detail carries what the check found when it passed — a resolved path, a
	// version string. The readiness panel shows it so a green row is evidence
	// rather than an assertion.
	Detail string

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

// Billing says whether a turn is charged per token or covered by a plan.
//
// This distinction is the difference between a cost figure and a lie. An agent
// running under a ChatGPT or Claude subscription is not billed for what it
// used, so reporting a confident dollar total for it would be false — pi says
// as much itself, printing "$0.067 (sub)" rather than a charge. Subscription
// turns are still costed at API rates, because comparing roles against each
// other is useful; they are simply labelled as estimates.
type Billing string

const (
	BillingUnknown      Billing = ""
	BillingMetered      Billing = "metered"
	BillingSubscription Billing = "subscription"
)

// EventKind is the vocabulary the cockpit renders. Adapters map their harness's
// native output onto it; the UI knows nothing about any specific harness.
type EventKind string

const (
	EventReady    EventKind = "ready"     // agent booted and is accepting work
	EventThinking EventKind = "thinking"  // reasoning, no tool call yet
	EventToolCall EventKind = "tool_call" // invoked a tool
	EventToolDone EventKind = "tool_done" // tool returned
	EventMessage  EventKind = "message"   // assistant prose
	EventUsage    EventKind = "usage"     // tokens and cost for a turn
	EventTurnEnd  EventKind = "turn_end"  // finished a turn, likely idle now
	EventError    EventKind = "error"     // harness-level failure, carries Fatal
)

type Event struct {
	Kind EventKind
	Role string
	At   time.Time

	Text string // prose or tool name, depending on Kind
	Tool string
	Args map[string]any

	// Usage keeps input split three ways because the prices differ by roughly
	// 50x and summing them into one "input tokens" figure misstates cost by an
	// order of magnitude (ARCHITECTURE.md §11.4).
	TokensIn         int // uncached input
	CacheReadTokens  int // ~0.1x
	CacheWriteTokens int // 1.25x at 5m TTL, 2x at 1h
	TokensOut        int
	CostUSD          float64

	// Model is what the harness reports it actually used, which is not always
	// what was asked for — a fallback or an alias resolves here.
	Model string

	// Provider is who served the turn. A single harness can front several:
	// pi reports openai-codex, anthropic, google and more, and spend is only
	// attributable per provider if the adapter says which one ran.
	Provider string

	// Billing is how this turn is charged. See Billing.
	Billing Billing

	// CostReported distinguishes a cost the harness stated from one zerg would
	// have to derive from a price table. Storing which is which keeps a
	// disagreement visible instead of averaging it away.
	CostReported bool

	// Fatal marks an error the agent cannot recover from, so the cerebrate
	// stops instead of leaving a process that looks alive and answers nothing.
	// The predecessor's codex agents sat at a prompt for 20 minutes returning
	// HTTP 400 on every turn, indistinguishable from working.
	Fatal bool
}
