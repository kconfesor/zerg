// Package piharness adapts the `pi` CLI.
//
// pi is the second harness, and building it is what turned the Adapter
// interface from a description of claude into an actual contract. Three fields
// the design required — provider, billing mode, and whether cost was reported
// or derived — were unexercised until something multi-provider and
// subscription-billed had to fill them.
//
// Every shape decoded here was captured from a running pi rather than recalled:
// originally 0.74.2, re-checked against 0.84.3.
package piharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
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

// ListModels unions pi's two catalogs, because either one can be incomplete.
//
// `pi --list-models` prints a fixed-column table; ~/.pi/agent/models-store.json
// is what its own picker reads. On pi 0.74.2 they disagreed in both directions:
// the table carried gpt-5.1 through gpt-5.3-codex, which the store omitted, and
// the store carried gpt-5.6-sol, -luna and -terra, which the table omitted
// despite their working when passed to --model. Twelve and ten entries, sixteen
// between them.
//
// On 0.84.3 they agree, so the merge is currently a no-op. It is kept because
// the disagreement was real and cost a person models they could select in pi
// itself, and because a union of two sources is only wrong if a source lies —
// which neither does. Both are read and merged, keyed by provider/id. The store
// wins on metadata: it is JSON with an exact context window, where the table
// rounds to a "272K" column that has to be parsed back.
//
// Either source failing degrades to whatever the other returned, and both
// failing to an empty catalog rather than an error — the role editor accepts
// free text, so an unlisted model is typed rather than unreachable.
func (*Adapter) ListModels(ctx context.Context) ([]adapter.Model, error) {
	var table []adapter.Model
	// CombinedOutput, not Output: pi prints the table on stderr. Reading only
	// stdout silently yields an empty catalog, which then degrades to "no
	// catalog" and looks like a working check.
	//
	// No --no-extensions here. Extensions can register providers, and the flag
	// would hide models the person can otherwise select.
	if out, err := exec.CommandContext(ctx, binary, "--list-models").CombinedOutput(); err == nil {
		table = parseModelTable(out)
	}
	return mergeCatalogs(table, modelsFromStore()), nil
}

// mergeCatalogs unions the two sources, keyed by provider/id.
//
// Separate from ListModels so it can be tested without a pi on the machine —
// the merge is the part with a rule in it, and shelling out is not.
func mergeCatalogs(table, store []adapter.Model) []adapter.Model {
	merged := map[string]adapter.Model{}
	var order []string

	add := func(m adapter.Model, authoritative bool) {
		prev, seen := merged[m.ID]
		if !seen {
			order = append(order, m.ID)
			merged[m.ID] = m
			return
		}
		// Keep the better-populated record rather than the later one: a store
		// entry missing a context window should not blank one the table had.
		if authoritative {
			if m.Label == "" {
				m.Label = prev.Label
			}
			if m.Context == 0 {
				m.Context = prev.Context
			}
			merged[m.ID] = m
		}
	}

	for _, m := range table {
		add(m, false)
	}
	for _, m := range store {
		add(m, true)
	}

	models := make([]adapter.Model, 0, len(order))
	for _, id := range order {
		models = append(models, merged[id])
	}
	return models
}

// storeModel is the subset of ~/.pi/agent/models-store.json that is needed.
// Decoding only these fields means pi adding a key does not break the read.
type storeModel struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ContextWindow int    `json:"contextWindow"`
}

// modelsFromStore reads the catalog pi's own model picker uses.
//
// Silent on every failure. The file is pi's, not zerg's: it is absent before
// pi is first authenticated and its shape is not a published contract, so a
// change here should cost the models it would have added and nothing else.
func modelsFromStore() []adapter.Model {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "models-store.json"))
	if err != nil {
		return nil
	}

	// Keyed by provider, each holding its own model list.
	var store map[string]struct {
		Models []storeModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil
	}

	// Map iteration is random and this list is shown to a person, so the
	// providers are sorted. Within a provider pi's own order is kept.
	providers := make([]string, 0, len(store))
	for p := range store {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	var models []adapter.Model
	for _, p := range providers {
		for _, m := range store[p].Models {
			if m.ID == "" {
				continue
			}
			provider := m.Provider
			if provider == "" {
				provider = p
			}
			models = append(models, adapter.Model{
				ID:       provider + "/" + m.ID,
				Label:    m.ID,
				Provider: provider,
				Context:  m.ContextWindow,
			})
		}
	}
	return models
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

// ── provider limits ───────────────────────────────────────────────────────

// pi's Codex provider composes "You have hit your ChatGPT usage limit (plus
// plan). Try again in ~47 min." from the error's plan_type and resets_at, and
// falls through to the raw codes for everything else. Both are matched: the
// sentence is what reaches stdout, the codes are what appear when a raw error
// body is passed through.
//
// Read from pi's own openai-codex-responses.js rather than recalled.
var (
	piLimit = regexp.MustCompile(
		`(?i)usage_limit_reached|usage_not_included|rate_limit_exceeded|hit your .* usage limit|\b429\b`)

	// "Try again in ~47 min."
	piRetryIn = regexp.MustCompile(`(?i)try again in\s+~?\s*(\d+)\s*(second|minute|hour|min|hr|sec)s?`)

	// resets_at is a unix second, and survives when a raw error body is what
	// reached the stream.
	piResetsAt = regexp.MustCompile(`"resets_at"\s*:\s*(\d{9,11})`)
)

// ThrottledBy recognises the provider refusing work on quota.
func (*Adapter) ThrottledBy(text string) (adapter.Throttle, bool) {
	if !piLimit.MatchString(text) {
		return adapter.Throttle{}, false
	}
	return adapter.Throttle{
		Until:  piResetAt(text, time.Now()),
		Detail: firstLine(text),
	}, true
}

func piResetAt(text string, now time.Time) time.Time {
	// The absolute stamp is preferred: a relative "~47 min" was already
	// rounded by pi from this same value.
	if m := piResetsAt.FindStringSubmatch(text); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if at := time.Unix(sec, 0); at.After(now) {
				return at
			}
		}
	}
	m := piRetryIn.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ── subscription quota ────────────────────────────────────────────────────

// chatgptUsageURL is the endpoint the ChatGPT app itself uses for the usage
// meter. Undocumented, and reached with the OAuth token pi already holds.
//
// Nothing in pi's own output carries these numbers — its /usage command
// reports session tokens, not the plan's remaining headroom — so unlike claude,
// which emits them unprompted every turn, they have to be asked for.
//
// The shape and the headers were read from the pi-chatgpt-limit extension
// (github.com/patlux/pi-chatgpt-limit), which does the same thing inside pi,
// and verified against a live account before being relied on here.
const chatgptUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// quotaHTTP is separate so a hung endpoint cannot hold a status refresh open.
var quotaHTTP = &http.Client{Timeout: 15 * time.Second}

// Quota reports what the ChatGPT plan has left.
//
// Every failure is soft: this is a gauge, and a missing gauge must never stop
// a role from running. An account without a plan — an API key rather than a
// subscription — returns false, which is not an error.
func (*Adapter) Quota(ctx context.Context) (adapter.Quota, bool, error) {
	tok, account, ok := codexToken()
	if !ok {
		return adapter.Quota{}, false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgptUsageURL, nil)
	if err != nil {
		return adapter.Quota{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zerg")
	if account != "" {
		req.Header.Set("chatgpt-account-id", account)
	}

	resp, err := quotaHTTP.Do(req)
	if err != nil {
		return adapter.Quota{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return adapter.Quota{}, false, fmt.Errorf("chatgpt usage: %s", resp.Status)
	}

	var body struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   usageWindow `json:"primary_window"`
			Secondary usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return adapter.Quota{}, false, fmt.Errorf("chatgpt usage: %w", err)
	}

	q := adapter.Quota{Plan: body.PlanType}
	// Which of the two is the five-hour and which the week is not fixed: on a
	// live account the primary window was the 7-day one. The length is the
	// only reliable identity, so neither position nor name is trusted.
	for _, w := range []usageWindow{body.RateLimit.Primary, body.RateLimit.Secondary} {
		if w.LimitWindowSeconds == nil || w.UsedPercent == nil {
			continue
		}
		out := adapter.QuotaWindow{
			Window: time.Duration(*w.LimitWindowSeconds) * time.Second,
			Used:   *w.UsedPercent / 100, // reported 0..100, carried 0..1
		}
		if w.ResetAt != nil && *w.ResetAt > 0 {
			out.ResetsAt = time.Unix(*w.ResetAt, 0)
		}
		q.Windows = append(q.Windows, out)
	}
	if len(q.Windows) == 0 {
		return adapter.Quota{}, false, nil
	}
	sort.Slice(q.Windows, func(i, j int) bool { return q.Windows[i].Window < q.Windows[j].Window })
	return q, true, nil
}

// usageWindow uses pointers so an absent field is distinguishable from a
// reported zero — "0% used" and "not reported" are different facts, and
// showing an empty bar for the second is a lie.
type usageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

// codexToken reads pi's stored OAuth credentials for the ChatGPT provider.
//
// Read-only, and never logged or returned anywhere it could be rendered: this
// is the operator's account token, held only long enough to sign one request.
func codexToken() (token, account string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "auth.json"))
	if err != nil {
		return "", "", false
	}
	var auth map[string]struct {
		Type      string `json:"type"`
		Access    string `json:"access"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return "", "", false
	}
	c, present := auth["openai-codex"]
	if !present || c.Type != "oauth" || c.Access == "" {
		return "", "", false
	}
	return c.Access, c.AccountID, true
}
