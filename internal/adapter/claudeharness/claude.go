// Package claudeharness adapts the `claude` CLI.
//
// It runs headless with structured input and output, so the orchestrator reads
// tool calls, usage and errors as typed events instead of scraping a terminal.
// Every flag and every field decoded here was verified against claude 2.1.243
// rather than recalled.
package claudeharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

const (
	binary       = "claude"
	providerName = "anthropic"
)

type Adapter struct {
	// billing is latched from the init event and read when usage arrives, so
	// it is written and read on the same stream but not necessarily the same
	// goroutine.
	billing atomic.Pointer[string]

	// model is latched the same way, because the result event that carries a
	// turn's usage does not name the model that produced it. Without this every
	// usage row stored an empty model, and grouping spend by model — one of the
	// three questions the dashboard exists to answer — returned a single
	// nameless bucket.
	//
	// The value comes from the assistant messages of the turn rather than from
	// the requested alias: "sonnet" is what was asked for, "claude-sonnet-5" is
	// what actually ran, and an alias that silently resolves elsewhere is
	// precisely what this should make visible.
	model atomic.Pointer[string]
}

func New() *Adapter {
	a := &Adapter{}
	unknown := string(adapter.BillingUnknown)
	a.billing.Store(&unknown)
	empty := ""
	a.model.Store(&empty)
	return a
}

// latchModel records the model actually serving this session. Called wherever
// the harness names one, since the result event does not.
func (a *Adapter) latchModel(model string) {
	if model != "" {
		a.model.Store(&model)
	}
}

// billingFor reads the CLI's own account of how it authenticated. An OAuth
// login ("none" — no API key involved) is a Claude plan, where usage is not
// charged per token, so its dollar figures are estimates rather than bills.
func billingFor(apiKeySource string) adapter.Billing {
	switch apiKeySource {
	case "":
		return adapter.BillingUnknown
	case "none":
		return adapter.BillingSubscription
	default:
		return adapter.BillingMetered
	}
}

func (*Adapter) Name() string { return "claude" }

func (*Adapter) Capabilities() adapter.Caps {
	return adapter.Caps{
		StructuredOutput: true,
		// --input-format stream-json accepts messages on stdin while the process
		// keeps running, so chat and clarification answers reach a live agent.
		StructuredInput: true,
		InteractiveTUI:  true,
		// Verified on macOS: credentials live in the keychain under
		// "Claude Code-credentials", and setting CLAUDE_CONFIG_DIR makes the
		// CLI report "Not logged in", even with .claude.json copied across.
		PrivateConfigDir: false,
		SystemPromptFile: true,
		ModelFlag:        true,
		ResumeSession:    true,
	}
}

// Command builds the headless invocation.
//
// --print with both stream-json formats is the bidirectional mode: the process
// stays up, reads turns from stdin and writes events to stdout. No prompt is
// passed as an argument, because work arrives over the pipe.
func (*Adapter) Command(ctx context.Context, spec adapter.Spec) (*exec.Cmd, error) {
	if spec.Worktree == "" {
		return nil, fmt.Errorf("claude: a role needs a worktree to run in")
	}
	if spec.SystemFile != "" {
		if _, err := os.Stat(spec.SystemFile); err != nil {
			return nil, fmt.Errorf("claude: composed system prompt %s: %w", spec.SystemFile, err)
		}
	}

	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // required for stream-json to emit anything but the result
	}

	// --strict-mcp-config and --permission-mode are no longer forced here.
	// They are the recommended defaults in settings, where they can be read and
	// changed, rather than facts of the adapter that only a rebuild can alter.
	// What each is for is documented beside those defaults.
	if spec.SystemFile != "" {
		args = append(args, "--append-system-prompt-file", spec.SystemFile)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// A last-resort floor, not a policy. An agent running unattended with no
	// permission setting at all will stop at the first prompt and look alive
	// while doing nothing, which is the failure this whole project is about —
	// so if settings have removed both switches, put one back.
	if !hasFlag(spec.ExtraArgs, "--permission-mode") &&
		!hasFlag(spec.ExtraArgs, "--dangerously-skip-permissions") {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	args = append(args, spec.ExtraArgs...)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = spec.Worktree
	var extra []string
	if spec.ConfigDir != "" {
		// A private config directory keeps two agents from racing a shared
		// global one. Two codex processes doing exactly that produced a config
		// file holding three concatenated copies of itself, which then failed to
		// parse for every invocation on the machine.
		extra = append(extra, "CLAUDE_CONFIG_DIR="+spec.ConfigDir)
	}
	cmd.Env = adapter.AgentEnv(spec, extra...)

	return cmd, nil
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// ── models ────────────────────────────────────────────────────────────────

// ListModels returns the aliases claude documents on --model, plus whatever the
// CLI has cached locally.
//
// There is no --list-models to ask, so this list is a convenience for the
// picker and never a gate: the role editor accepts free text, because a
// working model can be absent from any catalog. gpt-5.6-sol was missing from
// pi's list and ran fine; the failure mode worth preventing is the opposite
// one, where a hand-typed id 400s on every turn while the agent looks alive.
func (*Adapter) ListModels(_ context.Context) ([]adapter.Model, error) {
	models := []adapter.Model{
		{ID: "fable", Label: "Fable", Provider: "anthropic"},
		{ID: "opus", Label: "Opus", Provider: "anthropic"},
		{ID: "sonnet", Label: "Sonnet", Provider: "anthropic"},
		{ID: "haiku", Label: "Haiku", Provider: "anthropic"},
	}
	models = append(models, cachedModels()...)
	return models, nil
}

// cachedModels reads the CLI's own model cache. Best effort: a missing or
// unreadable cache means the alias list stands alone, not that anything failed.
func cachedModels() []adapter.Model {
	path, err := userConfigJSON()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Options []struct {
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"additionalModelOptionsCache"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []adapter.Model
	for _, o := range doc.Options {
		if o.Value == "" {
			continue
		}
		out = append(out, adapter.Model{ID: o.Value, Label: o.Label, Provider: "anthropic"})
	}
	return out
}

func userConfigJSON() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// ── parsing ───────────────────────────────────────────────────────────────

// wire mirrors the subset of claude's stream-json output zerg consumes.
type wire struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`

	Message struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
			// tool_result carries is_error, which is how a failed command is
			// distinguished from a successful one that printed to stderr.
			IsError bool `json:"is_error"`
		} `json:"content"`
		Usage usage `json:"usage"`
	} `json:"message"`

	// apiKeySource is how the CLI authenticated. "none" means an OAuth login,
	// i.e. a Claude plan, where tokens are not billed per use — so a confident
	// dollar figure would be a fiction. Anything else is a metered API key.
	APIKeySource string `json:"apiKeySource"`

	Usage        usage   `json:"usage"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	APIError     any     `json:"api_error_status"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u usage) empty() bool {
	return u.InputTokens == 0 && u.CacheCreationInputTokens == 0 &&
		u.CacheReadInputTokens == 0 && u.OutputTokens == 0
}

// Parse maps one output line onto zero or more typed events.
//
// Most lines are noise — hook chatter, rate-limit notices, banners — and return
// nothing. That is not an error condition, and treating it as one is how a
// harness upgrade turns into a wall of spurious failures.
func (a *Adapter) Parse(line []byte) ([]adapter.Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, nil
	}

	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, fmt.Errorf("claude: unreadable output line: %w", err)
	}

	switch w.Type {
	case "system":
		if w.Subtype == "init" {
			// The init event is the only place the CLI states how it
			// authenticated, so billing mode is latched here for the session.
			b := string(billingFor(w.APIKeySource))
			a.billing.Store(&b)
			a.latchModel(w.Model)
			return []adapter.Event{{
				Kind: adapter.EventReady, Model: w.Model,
				Provider: providerName, Billing: billingFor(w.APIKeySource),
			}}, nil
		}
		return nil, nil // hook_started, hook_response, and friends

	case "assistant":
		// The resolved model, which is not always the alias that was requested.
		a.latchModel(w.Message.Model)
		var out []adapter.Event
		for _, c := range w.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					out = append(out, adapter.Event{
						Kind: adapter.EventMessage, Text: c.Text,
						Model: w.Message.Model, Provider: providerName,
					})
				}
			case "tool_use":
				out = append(out, adapter.Event{
					Kind: adapter.EventToolCall, Tool: c.Name, Args: c.Input,
					Model: w.Message.Model, Provider: providerName,
				})
			case "thinking":
				out = append(out, adapter.Event{Kind: adapter.EventThinking, Model: w.Message.Model})
			}
		}
		// Per-message usage is cumulative within a turn; the authoritative
		// figure arrives with the result, so this is only emitted when a turn
		// ends without one.
		return out, nil

	case "user":
		var out []adapter.Event
		for _, c := range w.Message.Content {
			if c.Type == "tool_result" {
				out = append(out, adapter.Event{Kind: adapter.EventToolDone, Fatal: false, Text: boolLabel(c.IsError)})
			}
		}
		return out, nil

	case "result":
		out := []adapter.Event{{
			Kind:             adapter.EventUsage,
			Model:            deref(a.model.Load()),
			TokensIn:         w.Usage.InputTokens,
			CacheReadTokens:  w.Usage.CacheReadInputTokens,
			CacheWriteTokens: w.Usage.CacheCreationInputTokens,
			TokensOut:        w.Usage.OutputTokens,
			CostUSD:          w.TotalCostUSD,
			CostReported:     true,
			Provider:         providerName,
			Billing:          adapter.Billing(deref(a.billing.Load())),
		}}
		if w.IsError || w.APIError != nil {
			// A turn that errored must surface as an error, not as a quiet end.
			// Observed: agents returning HTTP 400 on every turn for twenty
			// minutes while looking perfectly alive.
			out = append(out, adapter.Event{
				Kind:  adapter.EventError,
				Text:  errorText(w),
				Fatal: isFatal(w),
			})
			return out, nil
		}
		// No text. result.Result is the turn's final assistant message, which
		// has already arrived as its own message event — carrying it again
		// stored every answer twice, in the tier that costs the most to keep.
		// turn_end means the turn ended; that is all anyone reads it for.
		out = append(out, adapter.Event{Kind: adapter.EventTurnEnd})
		return out, nil

	default:
		return nil, nil
	}
}

func errorText(w wire) string {
	if w.Result != "" {
		return w.Result
	}
	if w.APIError != nil {
		return fmt.Sprintf("api error: %v", w.APIError)
	}
	return "the harness reported an error without describing it"
}

// isFatal marks the errors a restart cannot fix, so a cerebrate stops instead
// of respawning into the same wall. A model the CLI is too old to call is the
// case that cost twenty minutes of a swarm looking healthy.
func isFatal(w wire) bool {
	text := strings.ToLower(errorText(w))
	for _, s := range []string{
		"requires a newer version",
		"invalid_request_error",
		"authentication",
		"unauthorized",
		"not found for provider",
	} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func boolLabel(isErr bool) string {
	if isErr {
		return "error"
	}
	return "ok"
}

// deref reads a latched string pointer, tolerating an unset value.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// EncodeTurn renders a turn as an SDK user message, the shape
// --input-format stream-json accepts. Verified against 2.1.243 by writing it
// to a live process and reading the answer back.
func (*Adapter) EncodeTurn(text string) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding turn: %w", err)
	}
	return append(b, '\n'), nil
}

// ── provider limits ───────────────────────────────────────────────────────

// claudeLimit matches what Claude Code prints when the subscription window is
// spent. Captured from the binary rather than recalled: its own fatal-error
// classifier carries "usage limit reached", and the surrounding copy is
// "resets in <duration>" or "until your limit resets at <time>".
var (
	// "until your limit resets" is the blocking phrasing, and is matched in
	// full. A bare "limit resets" is not: the binary also carries
	// "Lower-priority mode is offered again after your weekly limit resets",
	// which is informational, and pausing a working agent on it would be a
	// far worse failure than missing a throttle.
	claudeLimit = regexp.MustCompile(
		`(?i)usage limit reached|until your limit resets|rate_limit_error|\brate limit exceeded\b`)

	// "resets at 3pm", "resets at 15:00" — an absolute wall-clock time, which
	// is what the subscription copy uses.
	claudeResetsAt = regexp.MustCompile(`(?i)resets? at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

	// "resets in 42 minutes", "try again in ~3 hours".
	claudeResetsIn = regexp.MustCompile(`(?i)(?:resets?|try again) in\s+~?\s*(\d+)\s*(second|minute|hour|min|hr|sec)s?`)
)

// ThrottledBy recognises the subscription window being spent.
func (*Adapter) ThrottledBy(text string) (adapter.Throttle, bool) {
	if !claudeLimit.MatchString(text) {
		return adapter.Throttle{}, false
	}
	return adapter.Throttle{
		Until:  claudeResetAt(text, time.Now()),
		Detail: firstLine(text),
	}, true
}

// claudeResetAt reads whichever phrasing is present, and returns the zero time
// when neither is. A throttle with no known end is still a throttle; guessing
// an end would produce a role that says it resumes at a time it will not.
func claudeResetAt(text string, now time.Time) time.Time {
	if m := claudeResetsIn.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return time.Time{}
		}
		switch strings.ToLower(m[2]) {
		case "second", "sec":
			return now.Add(time.Duration(n) * time.Second)
		case "minute", "min":
			return now.Add(time.Duration(n) * time.Minute)
		case "hour", "hr":
			return now.Add(time.Duration(n) * time.Hour)
		}
		return time.Time{}
	}

	m := claudeResetsAt.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour > 23 {
		return time.Time{}
	}
	minute := 0
	if m[2] != "" {
		if minute, err = strconv.Atoi(m[2]); err != nil || minute > 59 {
			return time.Time{}
		}
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}

	at := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !at.After(now) {
		// "resets at 3pm" said at 4pm means tomorrow. A reset time in the past
		// would otherwise resume the role instantly, straight back into the wall.
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// firstLine keeps the harness's own sentence without the stack of context
// that can follow it.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
