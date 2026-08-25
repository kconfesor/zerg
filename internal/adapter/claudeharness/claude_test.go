package claudeharness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konfessor/zerg/internal/adapter"
)

// These fixtures are verbatim lines captured from claude 2.1.243 running
// `--print --output-format stream-json --verbose`. Parsing invented shapes
// would prove nothing about whether this adapter works.
const (
	lineHookStarted = `{"type":"system","subtype":"hook_started","hook_id":"ef7d","hook_name":"SessionStart:startup","session_id":"5ee7"}`
	lineInit        = `{"type":"system","subtype":"init","cwd":"/tmp/x","session_id":"5ee7","model":"claude-sonnet-5","permissionMode":"bypassPermissions","apiKeySource":"none"}`
	lineInitKeyed   = `{"type":"system","subtype":"init","session_id":"5ee7","model":"claude-opus-5","apiKeySource":"ANTHROPIC_API_KEY"}`
	lineAssistant   = `{"type":"assistant","message":{"model":"claude-sonnet-5","id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":14540,"cache_read_input_tokens":24326,"output_tokens":4}},"session_id":"5ee7"}`
	lineRateLimit   = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"},"session_id":"5ee7"}`
	lineResult      = `{"is_error":false,"duration_api_ms":1743,"num_turns":1,"stop_reason":"end_turn","session_id":"5ee7","total_cost_usd":0.0630692,"usage":{"input_tokens":2,"cache_creation_input_tokens":14540,"cache_read_input_tokens":24326,"output_tokens":4},"subtype":"success","api_error_status":null,"result":"ok","type":"result"}`
)

func parse(t *testing.T, line string) []adapter.Event {
	t.Helper()
	return parseWith(t, New(), line)
}

// parseWith keeps one adapter across several lines, which matters because
// billing mode is latched from the init event and read when usage arrives.
func parseWith(t *testing.T, a *Adapter, line string) []adapter.Event {
	t.Helper()
	evs, err := a.Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse(%.40s...): %v", line, err)
	}
	return evs
}

func TestParseInitIsReady(t *testing.T) {
	evs := parse(t, lineInit)
	if len(evs) != 1 || evs[0].Kind != adapter.EventReady {
		t.Fatalf("got %+v, want one ready event", evs)
	}
	if evs[0].Model != "claude-sonnet-5" {
		t.Errorf("model = %q; the ready event should report what the harness resolved", evs[0].Model)
	}
}

// Hook chatter and rate-limit notices are the bulk of the stream and carry no
// meaning. Treating unknown lines as errors is how a harness upgrade becomes a
// wall of spurious failures.
func TestParseIgnoresNoise(t *testing.T) {
	for _, line := range []string{lineHookStarted, lineRateLimit, "", "   ", "not json at all"} {
		if evs := parse(t, line); len(evs) != 0 {
			t.Errorf("line %.30q produced %+v, want nothing", line, evs)
		}
	}
}

func TestParseAssistantText(t *testing.T) {
	evs := parse(t, lineAssistant)
	if len(evs) != 1 || evs[0].Kind != adapter.EventMessage {
		t.Fatalf("got %+v, want one message event", evs)
	}
	if evs[0].Text != "ok" {
		t.Errorf("text = %q, want ok", evs[0].Text)
	}
}

// One assistant message can hold prose and several tool calls, which is why
// Parse returns a slice rather than a single event.
func TestParseAssistantWithToolCalls(t *testing.T) {
	line := `{"type":"assistant","message":{"model":"claude-opus-5","content":[
		{"type":"text","text":"running the tests"},
		{"type":"tool_use","name":"Bash","input":{"command":"cargo test"}},
		{"type":"tool_use","name":"Read","input":{"file_path":"src/lib.rs"}}]}}`

	evs := parse(t, line)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 from one message", len(evs))
	}
	if evs[0].Kind != adapter.EventMessage {
		t.Errorf("first event is %s, want message", evs[0].Kind)
	}
	if evs[1].Kind != adapter.EventToolCall || evs[1].Tool != "Bash" {
		t.Errorf("second event is %s/%s, want tool_call/Bash", evs[1].Kind, evs[1].Tool)
	}
	if got := evs[1].Args["command"]; got != "cargo test" {
		t.Errorf("tool args lost the command: %v", evs[1].Args)
	}
	if evs[2].Tool != "Read" {
		t.Errorf("third event tool = %q, want Read", evs[2].Tool)
	}
}

// The three-way input split is the whole reason usage is worth reading: the
// prices differ by roughly 50x, so summing them would misstate cost badly.
func TestParseResultCarriesTheCacheSplit(t *testing.T) {
	evs := parse(t, lineResult)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want usage then turn_end", len(evs))
	}

	u := evs[0]
	if u.Kind != adapter.EventUsage {
		t.Fatalf("first event is %s, want usage", u.Kind)
	}
	if u.TokensIn != 2 {
		t.Errorf("uncached input = %d, want 2", u.TokensIn)
	}
	if u.CacheWriteTokens != 14540 {
		t.Errorf("cache writes = %d, want 14540", u.CacheWriteTokens)
	}
	if u.CacheReadTokens != 24326 {
		t.Errorf("cache reads = %d, want 24326", u.CacheReadTokens)
	}
	if u.TokensOut != 4 {
		t.Errorf("output = %d, want 4", u.TokensOut)
	}
	if u.CostUSD < 0.06 || u.CostUSD > 0.07 {
		t.Errorf("cost = %v, want the reported 0.0630692", u.CostUSD)
	}

	if evs[1].Kind != adapter.EventTurnEnd {
		t.Errorf("second event is %s, want turn_end", evs[1].Kind)
	}
}

// A turn that errors must surface as an error rather than a quiet end. The
// predecessor's agents returned HTTP 400 on every turn for twenty minutes while
// looking perfectly alive.
func TestParseResultErrorSurfaces(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"total_cost_usd":0,
		"result":"The 'gpt-5.6-sol' model requires a newer version of Codex.","usage":{}}`

	evs := parse(t, line)
	var errEv *adapter.Event
	for i := range evs {
		if evs[i].Kind == adapter.EventError {
			errEv = &evs[i]
		}
	}
	if errEv == nil {
		t.Fatalf("an errored result produced %+v with no error event", evs)
	}
	// A CLI too old for its model cannot be fixed by restarting into it.
	if !errEv.Fatal {
		t.Error("a version rejection must be fatal, or the cerebrate respawns into the same wall")
	}
}

func TestParseToolResult(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"exit 101"}]}}`
	evs := parse(t, line)
	if len(evs) != 1 || evs[0].Kind != adapter.EventToolDone {
		t.Fatalf("got %+v, want one tool_done", evs)
	}
	if evs[0].Text != "error" {
		t.Errorf("a failed tool result should say so, got %q", evs[0].Text)
	}
}

// ── command ───────────────────────────────────────────────────────────────

func TestCommandUsesBidirectionalStructuredMode(t *testing.T) {
	dir := t.TempDir()
	sysFile := filepath.Join(dir, "system.md")
	if err := os.WriteFile(sysFile, []byte("be useful"), 0o644); err != nil {
		t.Fatalf("writing system file: %v", err)
	}

	cmd, err := New().Command(context.Background(), adapter.Spec{
		Role: "coder", Worktree: dir, Model: "opus", SystemFile: sysFile,
		Socket: "/tmp/zerg.sock", Token: "tok", ConfigDir: filepath.Join(dir, "cfg"),
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--print",
		"--input-format stream-json",  // input too: chat reaches a live agent
		"--output-format stream-json", // typed events instead of a screen
		"--verbose",                   // without it stream-json emits only the result
		"--model opus",
		"--append-system-prompt-file " + sysFile,
		"--permission-mode bypassPermissions",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("command is missing %q\ngot: %s", want, args)
		}
	}
	if cmd.Dir != dir {
		t.Errorf("cwd = %q, want the role's worktree %q", cmd.Dir, dir)
	}

	env := strings.Join(cmd.Env, "\n")
	// A private config directory is what stops two agents racing one global
	// file, which is how a shared config ended up holding three copies of itself.
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "cfg")) {
		t.Error("the agent was not given a private config directory")
	}
	for _, want := range []string{"ZERG_SOCKET=/tmp/zerg.sock", "ZERG_TOKEN=tok", "ZERG_ROLE=coder"} {
		if !strings.Contains(env, want) {
			t.Errorf("env is missing %q", want)
		}
	}
}

// An operator who sets a permission mode explicitly means it, so the default
// must not be appended on top of their choice.
func TestCommandDoesNotOverrideAnExplicitPermissionMode(t *testing.T) {
	dir := t.TempDir()
	cmd, err := New().Command(context.Background(), adapter.Spec{
		Role: "coder", Worktree: dir,
		ExtraArgs: []string{"--permission-mode", "acceptEdits"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if n := strings.Count(strings.Join(cmd.Args, " "), "--permission-mode"); n != 1 {
		t.Errorf("--permission-mode appears %d times, want 1", n)
	}
}

func TestCommandRejectsAMissingSystemFile(t *testing.T) {
	_, err := New().Command(context.Background(), adapter.Spec{
		Role: "coder", Worktree: t.TempDir(),
		SystemFile: filepath.Join(t.TempDir(), "absent.md"),
	})
	if err == nil {
		t.Fatal("a missing composed system prompt was accepted")
	}
}

func TestCommandRequiresAWorktree(t *testing.T) {
	if _, err := New().Command(context.Background(), adapter.Spec{Role: "coder"}); err == nil {
		t.Fatal("a role with no worktree was accepted")
	}
}

func TestCapabilitiesDeclareBidirectionalStreaming(t *testing.T) {
	caps := New().Capabilities()
	if !caps.StructuredOutput || !caps.StructuredInput {
		t.Error("claude supports streaming JSON in both directions; the caps must say so")
	}
	if !caps.SystemPromptFile {
		t.Error("--append-system-prompt-file exists and is verified; the caps must say so")
	}
}

func TestListModelsIncludesTheDocumentedAliases(t *testing.T) {
	models, err := New().ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	have := map[string]bool{}
	for _, m := range models {
		have[m.ID] = true
	}
	for _, want := range []string{"opus", "sonnet", "haiku"} {
		if !have[want] {
			t.Errorf("alias %q is missing from the model list", want)
		}
	}
}

// Billing comes from how the CLI authenticated, which it states once, in the
// init event. An OAuth login is a Claude plan: tokens are not charged per use,
// so a confident dollar total for that role would be a fiction.
func TestBillingIsLatchedFromTheInitEvent(t *testing.T) {
	a := New()
	if evs := parseWith(t, a, lineInit); evs[0].Billing != adapter.BillingSubscription {
		t.Fatalf("ready billing = %q, want subscription for an OAuth login", evs[0].Billing)
	}

	evs := parseWith(t, a, lineResult)
	if evs[0].Kind != adapter.EventUsage {
		t.Fatalf("first event is %s, want usage", evs[0].Kind)
	}
	if evs[0].Billing != adapter.BillingSubscription {
		t.Errorf("usage billing = %q; it must carry what init established", evs[0].Billing)
	}
	if evs[0].Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", evs[0].Provider)
	}
	if !evs[0].CostReported {
		t.Error("claude states total_cost_usd; that must be recorded rather than derived")
	}
}

func TestAnAPIKeyMeansMeteredBilling(t *testing.T) {
	a := New()
	if evs := parseWith(t, a, lineInitKeyed); evs[0].Billing != adapter.BillingMetered {
		t.Fatalf("billing = %q, want metered when an API key is in use", evs[0].Billing)
	}
	if evs := parseWith(t, a, lineResult); evs[0].Billing != adapter.BillingMetered {
		t.Errorf("usage billing = %q, want metered", evs[0].Billing)
	}
}
