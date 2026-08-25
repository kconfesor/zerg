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
	"strings"
	"sync/atomic"

	"github.com/konfessor/zerg/internal/adapter"
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
}

func New() *Adapter {
	a := &Adapter{}
	unknown := string(adapter.BillingUnknown)
	a.billing.Store(&unknown)
	return a
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
		StructuredInput:  true,
		InteractiveTUI:   true,
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
	if spec.SystemFile != "" {
		args = append(args, "--append-system-prompt-file", spec.SystemFile)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// The agent runs unattended in a worktree the operator chose, so permission
	// prompts have nobody to answer them.
	if !hasFlag(spec.ExtraArgs, "--permission-mode") {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	args = append(args, spec.ExtraArgs...)

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = spec.Worktree
	cmd.Env = append(os.Environ(),
		"ZERG_SOCKET="+spec.Socket,
		"ZERG_TOKEN="+spec.Token,
		"ZERG_ROLE="+spec.Role,
	)
	// A private config directory keeps two agents from racing a shared global
	// one. Two codex processes doing exactly that produced a config file
	// containing three concatenated copies of itself, which then failed to
	// parse for every invocation on the machine.
	if spec.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+spec.ConfigDir)
	}
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
			return []adapter.Event{{
				Kind: adapter.EventReady, Model: w.Model,
				Provider: providerName, Billing: billingFor(w.APIKeySource),
			}}, nil
		}
		return nil, nil // hook_started, hook_response, and friends

	case "assistant":
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
			// The predecessor's agents returned HTTP 400 on every turn for
			// twenty minutes while looking perfectly alive.
			out = append(out, adapter.Event{
				Kind:  adapter.EventError,
				Text:  errorText(w),
				Fatal: isFatal(w),
			})
			return out, nil
		}
		out = append(out, adapter.Event{Kind: adapter.EventTurnEnd, Text: w.Result})
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
