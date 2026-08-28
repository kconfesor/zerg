// Package adapter defines the contract every agent harness must satisfy.
//
// Hardcoding backends into a validation set and a case expression means adding
// one is an edit to the launcher, in every place the set appears. Here a
// harness is a registered implementation of Adapter, and the orchestrator never
// names a specific CLI.
package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
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
	Role     string
	Worktree string // agent cwd; a git worktree owned by this role
	Model    string
	// Thinking is the reasoning level, in the harness's own vocabulary. Empty
	// leaves the harness's default alone. The two shipped harnesses spell it
	// differently and offer different levels, which is why this is passed
	// through rather than translated: claude takes --effort, pi --thinking, and
	// only pi has "off" and "minimal".
	Thinking  string
	ExtraArgs []string

	// Env is extra variables this particular agent needs, as NAME=value.
	//
	// For the agents that are given a job rather than a queue: a runner is
	// told which port to bind, because the daemon is proxying it and two
	// projects both defaulting to 5173 would collide. Added after the
	// allowlist, so it is what the caller asked for rather than what the
	// daemon's own shell happened to hold.
	Env []string

	// SystemFile is composed fresh at every spawn from the shared instructions
	// plus this role's prompt, both read from the database. Copying prompt files
	// into each worktree at creation time means edits made afterward reach
	// nobody: a config set to Rust once produced a Clojure implementation across
	// six agents, silently.
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
// envAllow is what an agent inherits by name.
//
// Everything else is dropped. The daemon runs in whatever shell you started it
// from, and that shell routinely holds cloud keys, database URLs and CI tokens
// that have nothing to do with writing code — an agent given the whole
// environment can reach every one of them, and the first sign would be
// something it did with them.
//
// Kept: what a process needs to run at all, what a harness needs to find its
// own credentials, and what a build needs to work in a repository.
var envAllow = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true,
	"LANG": true, "LC_ALL": true, "TZ": true, "TERM": true, "COLORTERM": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true,
	"NO_PROXY": true, "HTTP_PROXY": true, "HTTPS_PROXY": true,
	"no_proxy": true, "http_proxy": true, "https_proxy": true,
	// Toolchains an agent is likely to invoke inside a repository.
	"GOPATH": true, "GOMODCACHE": true, "GOCACHE": true, "GOFLAGS": true,
	"CARGO_HOME": true, "RUSTUP_HOME": true, "JAVA_HOME": true,
	"PNPM_HOME": true, "NODE_OPTIONS": true,
}

// envAllowPrefix keeps whole families rather than naming every member.
//
// The provider variables are here because zerg deliberately does not manage
// credentials (§2): a harness authenticates itself, and stripping the variable
// it authenticates with would break the agent to protect a secret it is
// supposed to have.
var envAllowPrefix = []string{
	"LC_",
	"ANTHROPIC_", "CLAUDE_", "OPENAI_", "GEMINI_", "GOOGLE_API", "GROQ_",
	"DEEPSEEK_", "MISTRAL_", "OPENROUTER_", "XAI_", "PI_",
	"XDG_",
}

// droppedOnce reports the names withheld from agents, once per process, so a
// harness that suddenly cannot see something has a place to look.
var droppedOnce sync.Once

func agentEnvAllows(name string) bool {
	if envAllow[name] {
		return true
	}
	for _, p := range envAllowPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func AgentEnv(spec Spec, extra ...string) []string {
	path := os.Getenv("PATH")
	if spec.BinDir != "" {
		path = spec.BinDir + string(os.PathListSeparator) + path
	}

	out := make([]string, 0, 32+len(extra))
	var dropped []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "PATH" {
			continue
		}
		if !agentEnvAllows(name) {
			dropped = append(dropped, name)
			continue
		}
		out = append(out, kv)
	}
	if len(dropped) > 0 {
		droppedOnce.Do(func() {
			sort.Strings(dropped)
			slog.Debug("agents do not inherit these environment variables",
				"names", strings.Join(dropped, " "))
		})
	}

	out = append(out,
		"PATH="+path,
		"ZERG_SOCKET="+spec.Socket,
		"ZERG_TOKEN="+spec.Token,
		"ZERG_ROLE="+spec.Role,
	)
	out = append(out, spec.Env...)
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

	// Thinking lists the reasoning levels this harness accepts, weakest first,
	// for the picker in the role editor. Empty means the harness has no such
	// control and the field is not offered for it.
	Thinking []string

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

// Check is one preflight probe. Every incident here that was worth a postmortem
// — a stale CLI rejecting its model, a corrupted config, an unanswered trust
// dialog, a broken plugin tree — was a Check that did not exist yet.
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
	EventQuota    EventKind = "quota"     // subscription usage, carries Quota
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

	// Quota is the subscription's remaining headroom, when the harness
	// reported it. See Quota.
	Quota *Quota

	// Fatal marks an error the agent cannot recover from, so the cerebrate
	// stops instead of leaving a process that looks alive and answers nothing.
	// Observed: codex agents sitting at a prompt for 20 minutes returning HTTP
	// 400 on every turn, indistinguishable from working.
	Fatal bool
}

// ── provider limits ───────────────────────────────────────────────────────

// Throttle is a provider refusing work until a quota window rolls over.
//
// It is deliberately not an error: nothing is wrong with the agent, the code
// or the task, and the correct response is to wait rather than to investigate.
// Treating it as a crash costs the operator the twenty minutes it takes to
// discover that the thing to do was nothing.
type Throttle struct {
	// Until is when the quota is expected to lift. Zero when the harness said
	// it was throttled but not for how long — common, and not a reason to
	// discard the rest.
	Until time.Time

	// Detail is the harness's own sentence, kept verbatim. It names the plan
	// and the window in the provider's vocabulary, which is what a person
	// needs to decide whether to wait or switch models.
	Detail string
}

// QuotaWindow is one rolling limit and how much of it is spent.
//
// Windows are identified by their length rather than a name, because that is
// what the providers agree on: claude reports "five_hour" and "seven_day" as
// keys, the ChatGPT endpoint reports a primary and a secondary window with a
// limit_window_seconds each, and only the duration means the same thing in
// both.
type QuotaWindow struct {
	Window   time.Duration // 5h, 7d
	Used     float64       // 0..1
	ResetsAt time.Time     // zero when the provider did not say
}

// Label names a window by its length, for a person reading a bar.
func (w QuotaWindow) Label() string {
	switch {
	case w.Window >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(w.Window.Hours()/24))
	case w.Window >= time.Hour:
		return fmt.Sprintf("%dh", int(w.Window.Hours()))
	default:
		return w.Window.String()
	}
}

// Quota is what a subscription has left, as the provider states it.
//
// Separate from the throttle in Throttle: this is the gauge before the wall,
// and it is what stops a run being planned into a window that cannot hold it.
type Quota struct {
	// Provider is whose account these windows belong to — "openai-codex",
	// "anthropic". Not the harness: pi fronts several providers, and a
	// ChatGPT window says nothing about the deepseek key beside it.
	Provider string
	Plan     string // "prolite", "max", … when the provider names it
	Windows  []QuotaWindow
}

// Tightest returns the window closest to being spent, which is the one that
// will actually stop work. Ok is false when there are no windows.
func (q Quota) Tightest() (QuotaWindow, bool) {
	var out QuotaWindow
	found := false
	for _, w := range q.Windows {
		if !found || w.Used > out.Used {
			out, found = w, true
		}
	}
	return out, found
}

// QuotaReporter is implemented by adapters that have to ask for the figure
// rather than being told it.
//
// claude does not implement this: it emits the numbers unprompted on every
// turn, so asking would be a second way to learn the same thing. pi does,
// because nothing in its output carries them.
type QuotaReporter interface {
	// Quota reports the subscription's remaining headroom. The bool is false
	// when this role is not on a plan that has one — an API-key provider has
	// no window to report and is not an error.
	Quota(ctx context.Context) (Quota, bool, error)
}

// SessionScoped is implemented by adapters that keep per-session state.
//
// The registry holds one instance per harness, which is right for the parts
// that are configuration — capabilities, the model catalogue, how to build a
// command. It is wrong for anything latched from a running stream: claude
// records the model and billing mode a turn actually used, because the event
// carrying usage does not name them, and with one shared instance three
// concurrent claude roles overwrite each other's. No data race — the values are
// atomics — but every usage row can be attributed to whichever role wrote last,
// which silently corrupts the one number the cost dashboard exists to answer.
//
// Adapters with no such state do not implement this and are used as they are.
type SessionScoped interface {
	// NewSession returns an instance owned by one agent process.
	NewSession() Adapter
}

// forSession returns a private instance when the adapter needs one.
func ForSession(a Adapter) Adapter {
	if s, ok := a.(SessionScoped); ok {
		return s.NewSession()
	}
	return a
}

// Throttler is implemented by adapters that can recognise their harness
// refusing work on quota.
//
// Optional rather than part of Adapter: a harness that cannot tell a quota
// limit from any other failure should not be forced to pretend, and the type
// assertion keeps every existing implementation and test double compiling.
type Throttler interface {
	// ThrottledBy reports whether this output is a provider quota limit, and
	// when it lifts.
	ThrottledBy(text string) (Throttle, bool)
}
