package claudeharness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/adapter"
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

// A turn that errors must surface as an error rather than a quiet end. Observed:
// agents returning HTTP 400 on every turn for twenty minutes while looking
// perfectly alive.
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

// The result event carries a turn's usage but does not name the model that
// produced it, so the model is latched from the stream. Without this every
// usage row stored an empty model and grouping spend by model — one of the
// three questions the dashboard answers — returned one nameless bucket.
func TestUsageCarriesTheModelThatActuallyRan(t *testing.T) {
	a := New()

	// The alias that was requested is not necessarily what serves the turn, so
	// the assistant message is the authority.
	init := `{"type":"system","subtype":"init","model":"sonnet","apiKeySource":"none"}`
	if _, err := a.Parse([]byte(init)); err != nil {
		t.Fatalf("init: %v", err)
	}
	assistant := `{"type":"assistant","message":{"model":"claude-sonnet-5",
		"content":[{"type":"text","text":"working"}]}}`
	if _, err := a.Parse([]byte(assistant)); err != nil {
		t.Fatalf("assistant: %v", err)
	}

	result := `{"type":"result","subtype":"success","total_cost_usd":0.5,
		"usage":{"input_tokens":10,"output_tokens":20}}`
	events, err := a.Parse([]byte(result))
	if err != nil {
		t.Fatalf("result: %v", err)
	}

	var usage *adapter.Event
	for i := range events {
		if events[i].Kind == adapter.EventUsage {
			usage = &events[i]
		}
	}
	if usage == nil {
		t.Fatal("the result event produced no usage")
	}
	if usage.Model != "claude-sonnet-5" {
		t.Errorf("usage model = %q, want the model that ran, claude-sonnet-5", usage.Model)
	}
}

// turn_end says a turn ended. It used to also carry the turn's final message,
// which had already been emitted as its own event — so every answer was stored
// twice, in the tier with the shortest retention and the largest volume.
func TestTurnEndDoesNotRepeatTheFinalMessage(t *testing.T) {
	a := New()
	line := `{"type":"result","subtype":"success","total_cost_usd":0.1,
		"result":"the whole answer, again","usage":{"output_tokens":9}}`
	events, err := a.Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, e := range events {
		if e.Kind == adapter.EventTurnEnd && e.Text != "" {
			t.Errorf("turn_end carries %q; the message event already did", e.Text)
		}
	}
}

// The registry holds one adapter per harness, but claude latches the model a
// turn actually used — the result event carrying usage does not name it. Shared,
// three concurrent claude roles overwrite each other's latch and usage rows are
// attributed to whichever wrote last, which is not a data race and silently
// corrupts the number the cost dashboard exists to answer.
func TestEachSessionLatchesItsOwnModel(t *testing.T) {
	shared := New()

	a := adapter.ForSession(shared)
	b := adapter.ForSession(shared)
	if a == b {
		t.Fatal("two sessions received the same instance")
	}

	// Each reads its own stream naming a different model.
	for _, tc := range []struct {
		on    adapter.Adapter
		model string
	}{{a, "claude-opus-5"}, {b, "claude-sonnet-5"}} {
		line := `{"type":"assistant","message":{"model":"` + tc.model + `","content":[]}}`
		if _, err := tc.on.Parse([]byte(line)); err != nil {
			t.Fatalf("Parse: %v", err)
		}
	}

	// A usage event names the model that produced it, from that session's latch.
	usage := func(on adapter.Adapter) string {
		evs, err := on.Parse([]byte(`{"type":"result","subtype":"success","usage":{"output_tokens":1}}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		for _, e := range evs {
			if e.Kind == adapter.EventUsage {
				return e.Model
			}
		}
		return ""
	}

	if got := usage(a); got != "claude-opus-5" {
		t.Errorf("session A attributed usage to %q, want claude-opus-5", got)
	}
	if got := usage(b); got != "claude-sonnet-5" {
		t.Errorf("session B attributed usage to %q, want claude-sonnet-5", got)
	}
}

// The reasoning level reaches the CLI under the name that CLI uses.
//
// claude calls it --effort and pi calls it --thinking, and their level sets are
// not the same, which is why the level travels as the harness's own word rather
// than something translated in the middle.
func TestEffortReachesTheCommand(t *testing.T) {
	a := New()
	dir := t.TempDir()
	cmd, err := a.Command(context.Background(), adapter.Spec{Worktree: dir, Model: "opus", Thinking: "xhigh"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !slices.Contains(cmd.Args, "--effort") {
		t.Fatalf("no --effort in %v", cmd.Args)
	}
	for i, a := range cmd.Args {
		if a == "--effort" && cmd.Args[i+1] != "xhigh" {
			t.Errorf("--effort %s, want xhigh", cmd.Args[i+1])
		}
	}

	// And a role that says nothing about it leaves the harness's own default.
	cmd, err = a.Command(context.Background(), adapter.Spec{Worktree: dir})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "--effort") {
		t.Errorf("--effort passed for a role that set no level: %v", cmd.Args)
	}
	if !slices.Contains(a.Capabilities().Thinking, "xhigh") {
		t.Errorf("levels are %v, want the ones claude --help lists", a.Capabilities().Thinking)
	}
}

// A session to continue reaches the command as --resume, and never as
// --session-id.
//
// The distinction is measured rather than stylistic. Against claude 2.1.258,
// --session-id on a conversation that already exists exits before the process
// is up with "Session ID <uuid> is already in use", so the flag that creates a
// session cannot be used to continue one. Every id zerg holds came from claude
// itself and therefore names a session that exists.
func TestResumingASessionUsesResumeAndNotSessionID(t *testing.T) {
	a := New()
	dir := t.TempDir()
	cmd, err := a.Command(context.Background(), adapter.Spec{
		Worktree: dir, ResumeSession: "6f6a4877-35d6-4764-a23c-f5b8e02a0c6a",
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if slices.Contains(cmd.Args, "--session-id") {
		t.Errorf("--session-id would refuse a session claude has already written: %v", cmd.Args)
	}
	found := false
	for i, arg := range cmd.Args {
		if arg == "--resume" {
			found = true
			if got := cmd.Args[i+1]; got != "6f6a4877-35d6-4764-a23c-f5b8e02a0c6a" {
				t.Errorf("--resume %s, want the stored session id", got)
			}
		}
	}
	if !found {
		t.Errorf("no --resume in %v", cmd.Args)
	}

	// A role with nothing to resume starts a conversation of claude's own
	// choosing, which is what every spawn did before this existed.
	cmd, err = a.Command(context.Background(), adapter.Spec{Worktree: dir})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.Args, "--resume") {
		t.Errorf("--resume passed with no session to resume: %v", cmd.Args)
	}
}

// The session id is read out of the stream, not remembered from what was
// passed in.
//
// claude answers --resume on a session that is still live by forking to a new
// id and carrying on under that. Latching from the stream is what makes the
// stored id follow the conversation the work is actually going into; trusting
// the id zerg sent would resume a dead one on the next restart. Every frame
// carries session_id, so a fork announces itself on the frames after it.
func TestSessionIDIsLatchedFromTheStream(t *testing.T) {
	a := New()
	if a.SessionID() != "" {
		t.Errorf("a fresh adapter reported session %q before claude had said anything", a.SessionID())
	}

	if _, err := a.Parse([]byte(`{"type":"system","subtype":"init","model":"claude-opus-5",` +
		`"apiKeySource":"none","session_id":"first-session"}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.SessionID(); got != "first-session" {
		t.Errorf("SessionID = %q, want first-session", got)
	}

	// A fork mid-run, announced on an ordinary frame rather than on init.
	if _, err := a.Parse([]byte(`{"type":"assistant","session_id":"forked-session",` +
		`"message":{"model":"claude-opus-5","content":[]}}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.SessionID(); got != "forked-session" {
		t.Errorf("SessionID = %q after a fork, want forked-session", got)
	}

	// A frame with no session_id says nothing about the session and must not
	// erase what is known.
	if _, err := a.Parse([]byte(`{"type":"stream_event","event":{"type":"content_block_delta"}}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.SessionID(); got != "forked-session" {
		t.Errorf("SessionID = %q after a frame that named none, want forked-session", got)
	}
}

// A resume the CLI cannot honour says so in errors[], not in result, and that
// sentence is the only thing telling an operator what went wrong. Decoding it
// nowhere is how a swarm that could not come back reported "the harness
// reported an error without describing it" five times with a doubling backoff.
//
// The frame is quoted from claude 2.1.258, spawned by hand against a session id
// it had no transcript for.
func TestAResumeWithNoTranscriptSaysWhichSessionIsMissing(t *testing.T) {
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,
		"num_turns":0,"session_id":"1dc80989-7d59-48c1-a2ec-267f263158a0",
		"total_cost_usd":0,"usage":{},
		"errors":["No conversation found with session ID: 1dc80989-7d59-48c1-a2ec-267f263158a0"]}`

	var errEv *adapter.Event
	for _, ev := range parse(t, line) {
		if ev.Kind == adapter.EventError {
			e := ev
			errEv = &e
		}
	}
	if errEv == nil {
		t.Fatal("a failed resume produced no error event")
	}
	if !strings.Contains(errEv.Text, "No conversation found with session ID") {
		t.Errorf("error text = %q, want the reason the CLI gave", errEv.Text)
	}
	// Recoverable, but only by dropping the session: retrying the same resume
	// fails identically for ever, while stopping the role is more than the
	// situation deserves when a cold start would work.
	if !errEv.StaleSession {
		t.Error("a missing conversation must be marked stale, or the cerebrate resumes it again")
	}
	if errEv.Fatal {
		t.Error("a missing conversation is not fatal: a cold spawn recovers")
	}
}

// The generic fallback still has to exist for a frame that really says nothing.
func TestAnErrorWithNothingToSayKeepsTheGenericText(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"total_cost_usd":0,"usage":{}}`
	for _, ev := range parse(t, line) {
		if ev.Kind == adapter.EventError && ev.Text != "the harness reported an error without describing it" {
			t.Errorf("error text = %q, want the generic fallback", ev.Text)
		}
	}
}
