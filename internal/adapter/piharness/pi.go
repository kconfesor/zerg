// Package piharness adapts the `pi` CLI.
//
// pi is the second harness, and building it is what turned the Adapter
// interface from a description of claude into an actual contract. Three fields
// the design required — provider, billing mode, and whether cost was reported
// or derived — were unexercised until something multi-provider and
// subscription-billed had to fill them.
//
// Every shape decoded here was captured from pi 0.74.2 rather than recalled.
package piharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/konfessor/zerg/internal/adapter"
)

const binary = "pi"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return "pi" }

func (*Adapter) Capabilities() adapter.Caps {
	return adapter.Caps{
		StructuredOutput: true,
		// Verified: --mode rpc reads newline-delimited commands on stdin and
		// answers {"type":"response","command":"prompt","success":true} while
		// streaming the same events as --mode json.
		StructuredInput: true,
		InteractiveTUI:  true,
		// pi keeps credentials in auth.json inside its config directory, so a
		// private directory is safe once that file is seeded across.
		PrivateConfigDir: true,
		SystemPromptFile: true, // --append-system-prompt resolves a path
		ModelFlag:        true,
		ResumeSession:    true, // --session <path|id>
	}
}

// Command builds the headless invocation.
//
// --mode rpc is the bidirectional mode: the process stays up, takes turns as
// JSON on stdin, and streams events on stdout. No prompt is passed as an
// argument, because work arrives over the pipe.
func (*Adapter) Command(ctx context.Context, spec adapter.Spec) (*exec.Cmd, error) {
	if spec.Worktree == "" {
		return nil, fmt.Errorf("pi: a role needs a worktree to run in")
	}
	if spec.SystemFile != "" {
		if _, err := os.Stat(spec.SystemFile); err != nil {
			return nil, fmt.Errorf("pi: composed system prompt %s: %w", spec.SystemFile, err)
		}
	}

	args := []string{"--mode", "rpc"}
	if spec.SystemFile != "" {
		// --append-system-prompt takes either literal text or a path and
		// resolves the file itself; both were verified against 0.74.2.
		args = append(args, "--append-system-prompt", spec.SystemFile)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// Extensions are opt-out because a broken extension tree takes the whole
	// process down before it reads a single turn — which is exactly how pi
	// failed on this machine, with every bundled extension erroring on a
	// missing module. An orchestrated agent needs none of them.
	if !hasFlag(spec.ExtraArgs, "--no-extensions", "-ne") {
		args = append(args, "--no-extensions")
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
		//
		// It has to be seeded first: pi keeps credentials in auth.json inside
		// that directory, so an unseeded private directory means an agent that
		// launches with no way to reach any provider.
		if err := seedConfigDir(spec.ConfigDir); err != nil {
			return nil, err
		}
		extra = append(extra, "PI_CODING_AGENT_DIR="+spec.ConfigDir)
	}
	cmd.Env = adapter.AgentEnv(spec, extra...)

	return cmd, nil
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

// ── billing ───────────────────────────────────────────────────────────────

// subscriptionProviders are transports billed by plan rather than per token.
//
// openai-codex is the ChatGPT-plan path, and pi itself labels it: running a
// turn through it prints "$0.067 (sub)". Anything unlisted is treated as
// metered, which is the conservative direction — mislabelling a real charge as
// an estimate is a smaller error than presenting an estimate as a bill.
var subscriptionProviders = map[string]bool{
	"openai-codex": true,
}

func billingFor(provider string) adapter.Billing {
	if provider == "" {
		return adapter.BillingUnknown
	}
	if subscriptionProviders[provider] {
		return adapter.BillingSubscription
	}
	return adapter.BillingMetered
}

// ── models ────────────────────────────────────────────────────────────────

// ListModels shells out to `pi --list-models`, which prints a fixed-column
// table and ignores --mode json.
//
// Parsing a table is unpleasant, but the alternative is a hand-maintained list
// of every model across every provider pi fronts, which would be wrong within a
// week. A parse failure degrades to an empty catalog rather than an error: the
// role editor accepts free text anyway, and gpt-5.6-sol is itself absent from
// this table while working perfectly.
func (*Adapter) ListModels(ctx context.Context) ([]adapter.Model, error) {
	// CombinedOutput, not Output: pi prints the table on stderr. Reading only
	// stdout silently yields an empty catalog, which then degrades to "no
	// catalog" and looks like a working check.
	out, err := exec.CommandContext(ctx, binary, "--no-extensions", "--list-models").CombinedOutput()
	if err != nil {
		return nil, nil
	}
	return parseModelTable(out), nil
}

func parseModelTable(out []byte) []adapter.Model {
	var models []adapter.Model
	for _, raw := range bytes.Split(out, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		provider, id := fields[0], fields[1]
		// Skip the header and any warning lines that reached stdout.
		if provider == "provider" || strings.HasSuffix(provider, ":") {
			continue
		}
		models = append(models, adapter.Model{
			ID:       provider + "/" + id,
			Label:    id,
			Provider: provider,
			Context:  parseTokenCount(fieldAt(fields, 2)),
		})
	}
	return models
}

func fieldAt(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// parseTokenCount reads the table's "272K" style context column.
func parseTokenCount(s string) int {
	if s == "" {
		return 0
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1_000, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "M")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n * mult
}

// ── parsing ───────────────────────────────────────────────────────────────

// wire mirrors the subset of pi's event stream zerg consumes.
//
// Note how differently this is shaped from claude's: usage nests a cost
// breakdown, token fields are named input/output/cacheRead/cacheWrite, and the
// provider travels on every message. The four token concepts line up exactly,
// which is the part of the Event contract this adapter confirmed rather than
// changed.
type wire struct {
	Type    string `json:"type"`
	Message struct {
		Role     string `json:"role"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Content  []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage usage `json:"usage"`
	} `json:"message"`

	// rpc acknowledgements, which carry command failures.
	Command string `json:"command"`
	Success *bool  `json:"success"`
	Error   string `json:"error"`
}

type usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Cost       struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// Parse maps one output line onto zero or more typed events.
//
// pi streams a text_delta per token via message_update, which would drown the
// event log for no benefit — the completed message arrives anyway. Only
// terminal states are emitted.
func (*Adapter) Parse(line []byte) ([]adapter.Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, nil // startup warnings reach stdout too
	}

	var w wire
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, fmt.Errorf("pi: unreadable output line: %w", err)
	}

	switch w.Type {
	case "session":
		return []adapter.Event{{Kind: adapter.EventReady}}, nil

	case "message_end":
		if w.Message.Role != "assistant" {
			return nil, nil // the echoed user turn
		}
		var out []adapter.Event
		for _, c := range w.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					out = append(out, adapter.Event{
						Kind: adapter.EventMessage, Text: c.Text,
						Model: w.Message.Model, Provider: w.Message.Provider,
					})
				}
			case "toolCall", "tool_use":
				out = append(out, adapter.Event{
					Kind: adapter.EventToolCall, Tool: c.Name, Args: c.Input,
					Model: w.Message.Model, Provider: w.Message.Provider,
				})
			}
		}
		return out, nil

	case "turn_end":
		u := w.Message.Usage
		return []adapter.Event{
			{
				Kind:             adapter.EventUsage,
				TokensIn:         u.Input,
				CacheReadTokens:  u.CacheRead,
				CacheWriteTokens: u.CacheWrite,
				TokensOut:        u.Output,
				CostUSD:          u.Cost.Total,
				CostReported:     true,
				Model:            w.Message.Model,
				Provider:         w.Message.Provider,
				Billing:          billingFor(w.Message.Provider),
			},
			{Kind: adapter.EventTurnEnd, Model: w.Message.Model, Provider: w.Message.Provider},
		}, nil

	case "response":
		// An rpc command that failed is a real error: the turn zerg submitted
		// never ran, and treating that as silence is how a role sits idle
		// holding a lease it will never satisfy.
		if w.Success != nil && !*w.Success {
			return []adapter.Event{{
				Kind:  adapter.EventError,
				Text:  fmt.Sprintf("pi rejected the %s command: %s", w.Command, w.Error),
				Fatal: false,
			}}, nil
		}
		return nil, nil

	default:
		// agent_start, turn_start, message_start, message_update, agent_end
		return nil, nil
	}
}

func userConfigDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// EncodeTurn renders a turn as an rpc prompt command. Verified against 0.74.2:
// this shape answers {"type":"response","command":"prompt","success":true},
// while several plausible alternatives are rejected outright.
func (*Adapter) EncodeTurn(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{"type": "prompt", "message": text})
	if err != nil {
		return nil, fmt.Errorf("pi: encoding turn: %w", err)
	}
	return append(b, '\n'), nil
}

// seedConfigDir copies what a private config directory needs from the user's
// real one: credentials, settings, and the model cache.
//
// Only files that already exist are copied, and an existing copy is left
// alone — an agent may have written to its own directory since, and
// overwriting that on every spawn would discard it.
func seedConfigDir(dir string) error {
	source := userConfigDir()
	if source == "" || source == dir {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pi: creating %s: %w", dir, err)
	}

	for _, name := range []string{"auth.json", "settings.json", "models-store.json"} {
		dst := filepath.Join(dir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("pi: reading %s: %w", name, err)
		}
		// Credentials: readable by their owner and nobody else.
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return fmt.Errorf("pi: seeding %s: %w", name, err)
		}
	}
	return nil
}
