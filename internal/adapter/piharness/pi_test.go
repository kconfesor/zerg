package piharness

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Fixtures captured verbatim from pi 0.74.2 running `--mode json`. Note how
// little they resemble claude's stream: usage nests a cost breakdown, the
// token fields are named differently, and the provider rides on every message.
const (
	lineSession = `{"type":"session","version":3,"id":"01a0","timestamp":"2026-08-25T03:21:26.706Z","cwd":"/tmp/x"}`
	lineTurnEnd = `{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"api":"openai-codex-responses","provider":"openai-codex","model":"gpt-5.6-sol","usage":{"input":4196,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":4201,"cost":{"input":0.02098,"output":0.00015,"cacheRead":0,"cacheWrite":0,"total":0.02113}},"stopReason":"stop"},"toolResults":[]}`
	lineMsgEnd  = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"provider":"openai-codex","model":"gpt-5.6-sol","usage":{"input":4196,"output":5,"cacheRead":0,"cacheWrite":0}}}`
	lineUserEnd = `{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"do it"}]}}`
	lineRPCFail = `{"type":"response","command":"prompt","success":false,"error":"Cannot read properties of undefined"}`
	lineRPCOK   = `{"type":"response","command":"prompt","success":true}`
)

func parse(t *testing.T, line string) []adapter.Event {
	t.Helper()
	evs, err := New().Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse(%.40s...): %v", line, err)
	}
	return evs
}

func TestParseSessionIsReady(t *testing.T) {
	evs := parse(t, lineSession)
	if len(evs) != 1 || evs[0].Kind != adapter.EventReady {
		t.Fatalf("got %+v, want one ready event", evs)
	}
}

// This is the test the whole reorder was for. pi is multi-provider and
// subscription-billed, so it fills three fields claude never exercised.
func TestParseTurnEndCarriesProviderAndBilling(t *testing.T) {
	evs := parse(t, lineTurnEnd)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want usage then turn_end", len(evs))
	}

	u := evs[0]
	if u.Kind != adapter.EventUsage {
		t.Fatalf("first event is %s, want usage", u.Kind)
	}
	if u.Provider != "openai-codex" {
		t.Errorf("provider = %q; spend is only attributable if the adapter says who served the turn", u.Provider)
	}
	// openai-codex is the ChatGPT-plan transport, and pi itself labels its
	// cost "(sub)". Reporting that as a charge would be false.
	if u.Billing != adapter.BillingSubscription {
		t.Errorf("billing = %q, want subscription for openai-codex", u.Billing)
	}
	if !u.CostReported {
		t.Error("pi states its own cost; that must be recorded rather than derived")
	}

	// The four token concepts line up with claude's despite the different
	// names — the part of the contract this adapter confirmed rather than changed.
	if u.TokensIn != 4196 || u.TokensOut != 5 {
		t.Errorf("tokens in/out = %d/%d, want 4196/5", u.TokensIn, u.TokensOut)
	}
	if u.CostUSD < 0.021 || u.CostUSD > 0.0212 {
		t.Errorf("cost = %v, want the reported 0.02113", u.CostUSD)
	}
	if evs[1].Kind != adapter.EventTurnEnd {
		t.Errorf("second event is %s, want turn_end", evs[1].Kind)
	}
}

func TestBillingDefaultsToMeteredForUnknownProviders(t *testing.T) {
	// Mislabelling a real charge as an estimate is a smaller error than
	// presenting an estimate as a bill, so unknown providers are metered.
	if got := billingFor("anthropic"); got != adapter.BillingMetered {
		t.Errorf("anthropic billing = %q, want metered", got)
	}
	if got := billingFor(""); got != adapter.BillingUnknown {
		t.Errorf("empty provider billing = %q, want unknown", got)
	}
}

func TestParseAssistantMessage(t *testing.T) {
	evs := parse(t, lineMsgEnd)
	if len(evs) != 1 || evs[0].Kind != adapter.EventMessage {
		t.Fatalf("got %+v, want one message", evs)
	}
	if evs[0].Provider != "openai-codex" {
		t.Errorf("provider = %q, want it carried on the message too", evs[0].Provider)
	}
}

// pi echoes the submitted user turn back as a message_end. Emitting it would
// duplicate into the event log something zerg itself just sent.
func TestParseIgnoresTheEchoedUserTurn(t *testing.T) {
	if evs := parse(t, lineUserEnd); len(evs) != 0 {
		t.Errorf("the echoed user turn produced %+v, want nothing", evs)
	}
}

// The frames that say nothing: starts, ends, acknowledgements and the warnings
// pi prints around them.
func TestParseIgnoresStreamingNoise(t *testing.T) {
	for _, line := range []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_start","contentIndex":1}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"done"}}`,
		`{"type":"agent_end","messages":[]}`,
		lineRPCOK,
		`Warning: Model "gpt-5.6-sol" not found for provider "openai-codex".`,
		"",
	} {
		if evs := parse(t, line); len(evs) != 0 {
			t.Errorf("line %.40q produced %+v, want nothing", line, evs)
		}
	}
}

// A fragment of an answer being written is emitted, so a person watching sees
// the sentence appear.
//
// These used to be dropped here, on the reasoning that they would drown the
// event log -- which was true when nothing consumed them and there was nowhere
// to put them but the log. Keeping them out of the transcript is the
// recorder's job now, and it refuses them at the door; the bus carries them to
// whoever is watching and nobody writes them down.
func TestParseEmitsTextFragments(t *testing.T) {
	evs := parse(t, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"the quick"}}`)
	if len(evs) != 1 {
		t.Fatalf("got %+v, want one event", evs)
	}
	if evs[0].Kind != adapter.EventMessageDelta || evs[0].Text != "the quick" {
		t.Errorf("got %+v, want the fragment as a message_delta", evs[0])
	}

	// An empty one says nothing and is not worth waking a renderer for.
	if evs := parse(t, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":""}}`); len(evs) != 0 {
		t.Errorf("an empty fragment produced %+v", evs)
	}
}

// A rejected rpc command means the turn zerg submitted never ran. Treating that
// as silence leaves a role idle holding a lease it can never satisfy.
func TestParseSurfacesRejectedCommands(t *testing.T) {
	evs := parse(t, lineRPCFail)
	if len(evs) != 1 || evs[0].Kind != adapter.EventError {
		t.Fatalf("got %+v, want one error event", evs)
	}
	if !strings.Contains(evs[0].Text, "prompt") {
		t.Errorf("error should name the rejected command, got %q", evs[0].Text)
	}
}

// ── command ───────────────────────────────────────────────────────────────

func TestCommandUsesRPCAndDisablesExtensions(t *testing.T) {
	dir := t.TempDir()
	sysFile := filepath.Join(dir, "system.md")
	if err := os.WriteFile(sysFile, []byte("be useful"), 0o644); err != nil {
		t.Fatalf("writing system file: %v", err)
	}

	cmd, err := New().Command(context.Background(), adapter.Spec{
		Role: "cleaner", Worktree: dir, Model: "openai-codex/gpt-5.6-sol",
		SystemFile: sysFile, Socket: "/tmp/zerg.sock", Token: "tok",
		ConfigDir: filepath.Join(dir, "cfg"),
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}

	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"--mode rpc", // bidirectional: turns arrive on stdin
		"--model openai-codex/gpt-5.6-sol",
		"--append-system-prompt " + sysFile,
		// A broken extension tree takes the process down before it reads a
		// turn, which is exactly how pi failed on this machine.
		"--no-extensions",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("command is missing %q\ngot: %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "PI_CODING_AGENT_DIR="+filepath.Join(dir, "cfg")) {
		t.Error("the agent was not given a private config directory")
	}
}

func TestCommandDoesNotRepeatNoExtensions(t *testing.T) {
	cmd, err := New().Command(context.Background(), adapter.Spec{
		Role: "cleaner", Worktree: t.TempDir(), ExtraArgs: []string{"--no-extensions"},
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if n := strings.Count(strings.Join(cmd.Args, " "), "--no-extensions"); n != 1 {
		t.Errorf("--no-extensions appears %d times, want 1", n)
	}
}

func TestCapabilitiesMatchWhatWasVerified(t *testing.T) {
	caps := New().Capabilities()
	// Verified by driving rpc over a pipe: it answers
	// {"type":"response","command":"prompt","success":true} and streams events.
	if !caps.StructuredInput || !caps.StructuredOutput {
		t.Error("pi does streaming JSON both ways; the caps must say so")
	}
}

// ── model table ───────────────────────────────────────────────────────────

func TestParseModelTable(t *testing.T) {
	table := `provider      model                context  max-out  thinking  images
openai-codex  gpt-5.1              272K     128K     yes       yes
openai-codex  gpt-5.3-codex-spark  128K     128K     yes       no
anthropic     claude-opus-5        1M       64K      yes       yes
`
	models := parseModelTable([]byte(table))
	if len(models) != 3 {
		t.Fatalf("parsed %d models, want 3 (the header must be skipped)", len(models))
	}
	// pi addresses models as provider/id, which is why Model.ID is qualified
	// while claude's is a bare alias.
	if models[0].ID != "openai-codex/gpt-5.1" {
		t.Errorf("id = %q, want the qualified form", models[0].ID)
	}
	if models[0].Provider != "openai-codex" {
		t.Errorf("provider = %q", models[0].Provider)
	}
	if models[0].Context != 272000 {
		t.Errorf("context = %d, want 272000 parsed from 272K", models[0].Context)
	}
	if models[2].Context != 1000000 {
		t.Errorf("context = %d, want 1000000 parsed from 1M", models[2].Context)
	}
}

// A model absent from the table must warn, not block: gpt-5.6-sol is exactly
// that case, and it works.
func TestUnlistedModelWarnsRatherThanBlocks(t *testing.T) {
	a := New()
	var check adapter.Check
	for _, c := range a.Checks() {
		if c.Name == "model_available" {
			check = c
		}
	}
	if check.Run == nil {
		t.Fatal("model_available check is missing")
	}
	res := check.Run(context.Background(), adapter.Spec{Model: "openai-codex/definitely-not-real"})
	if !res.OK && !res.Warn {
		t.Errorf("an unlisted model blocked the role: %+v", res)
	}
}

// The union is the point: before it, whichever source was chosen lost models
// the person could select in pi itself. This asserts both directions of the
// disagreement, so dropping either source fails it.
func TestModelStoreAndTableAreMerged(t *testing.T) {
	table := parseModelTable([]byte(
		"provider      model      context\n" +
			"openai-codex  gpt-5.1    272K\n" +
			"openai-codex  gpt-5.4    272K\n"))

	store := []adapter.Model{
		{ID: "openai-codex/gpt-5.4", Label: "gpt-5.4", Provider: "openai-codex", Context: 272000},
		{ID: "openai-codex/gpt-5.6-sol", Label: "gpt-5.6-sol", Provider: "openai-codex", Context: 272000},
	}

	got := map[string]adapter.Model{}
	for _, m := range mergeCatalogs(table, store) {
		got[m.ID] = m
	}

	// gpt-5.1 exists only in the table, gpt-5.6-sol only in the store.
	for _, want := range []string{"openai-codex/gpt-5.1", "openai-codex/gpt-5.6-sol"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s missing: one source was dropped", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d models, want 3 (the overlap counted once)", len(got))
	}
	// The store's exact window beats the table's rounded column.
	if got["openai-codex/gpt-5.4"].Context != 272000 {
		t.Errorf("overlap kept the wrong record: %+v", got["openai-codex/gpt-5.4"])
	}
}

// The store is pi's file, not zerg's. A missing or malformed one costs the
// models it would have added and nothing else.
func TestModelStoreDegradesQuietly(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no store at all
	if got := modelsFromStore(); got != nil {
		t.Errorf("missing store returned %v, want nil", got)
	}
}

// pi continues a conversation with the flag that also creates one.
//
// Verified against pi 0.84.4: a second process given a --session-id it had
// already used appended to the same session file rather than refusing, which is
// why this needs none of the create-or-continue distinction claude does. The
// file is resolved under the working directory, and a role's working directory
// is its own worktree on every spawn.
func TestResumingASessionUsesSessionID(t *testing.T) {
	a := New()
	dir := t.TempDir()
	cmd, err := a.Command(context.Background(), adapter.Spec{
		Worktree: dir, ResumeSession: "9b526675-f043-4040-a58e-f5e2cbc844bd",
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	found := false
	for i, arg := range cmd.Args {
		if arg == "--session-id" {
			found = true
			if got := cmd.Args[i+1]; got != "9b526675-f043-4040-a58e-f5e2cbc844bd" {
				t.Errorf("--session-id %s, want the stored session id", got)
			}
		}
	}
	if !found {
		t.Errorf("no --session-id in %v", cmd.Args)
	}

	// A cold spawn names one, because pi will not tell zerg what it chose.
	//
	// The `session` frame goes into the session file, not onto stdout: in
	// --mode rpc pi announces nothing before its first turn. Leaving the id to
	// the stream meant no pi role ever recorded a conversation and every one of
	// them started cold on every restart, silently, with the conversation on
	// disk the whole time.
	fresh := New()
	cmd, err = fresh.Command(context.Background(), adapter.Spec{Worktree: dir})
	if err != nil {
		t.Fatal(err)
	}
	named := ""
	for i, arg := range cmd.Args {
		if arg == "--session-id" {
			named = cmd.Args[i+1]
		}
	}
	if named == "" {
		t.Fatalf("a cold spawn named no session: %v", cmd.Args)
	}
	if got := fresh.SessionID(); got != named {
		t.Errorf("SessionID() = %q, want the %q it just spawned with; the recorder "+
			"reads this and would store nothing", got, named)
	}
	// The shape pi uses for its own, so a session directory stays in creation
	// order and pi has nothing to object to.
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).
		MatchString(named) {
		t.Errorf("named session %q, want a version 7 UUID", named)
	}

	// Two spawns are two conversations. Sharing one would have a role resume
	// somebody else's.
	other, err := New().Command(context.Background(), adapter.Spec{Worktree: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range other.Args {
		if arg == "--session-id" && other.Args[i+1] == named {
			t.Error("two cold spawns were given the same session id")
		}
	}
}

// pi's own frame still wins, for a version that emits one.
//
// The id is named at spawn because 0.84.4 keeps that frame in the session file
// rather than on the stream. Parse overwriting it costs nothing and means a pi
// that starts announcing is believed rather than argued with.
func TestPiSessionIDIsLatchedFromTheStream(t *testing.T) {
	a := New()
	if a.SessionID() != "" {
		t.Errorf("a fresh adapter reported session %q before pi had said anything", a.SessionID())
	}
	evs, err := a.Parse([]byte(`{"type":"session","version":3,"id":"9b526675-f043",` +
		`"timestamp":"2026-09-02T01:45:37.747Z","cwd":"/tmp/work"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != adapter.EventReady {
		t.Fatalf("the session frame should still report ready, got %v", evs)
	}
	if got := a.SessionID(); got != "9b526675-f043" {
		t.Errorf("SessionID = %q, want 9b526675-f043", got)
	}
}
