package piharness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konfessor/zerg/internal/adapter"
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

// pi emits a text_delta per token. Those would drown the log for no gain, since
// the completed message follows.
func TestParseIgnoresStreamingNoise(t *testing.T) {
	for _, line := range []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"o"}}`,
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
