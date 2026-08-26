package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func usageDB(t *testing.T) (*DB, *Project) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	p, err := db.CreateProject(ctx, mustDir(t, "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return db, p
}

// Input is stored in three columns because the three are priced roughly 50x
// apart. A total that folds them together would report a cache-heavy turn and
// an uncached one as the same spend, which is the specific mistake this schema
// exists to prevent.
func TestUsageKeepsTheInputSplitIntact(t *testing.T) {
	ctx := context.Background()
	db, p := usageDB(t)

	turn := UsageTurn{
		ProjectID: p.ID, Role: "coder", Provider: "anthropic", Model: "sonnet",
		InputTokens: 34, CacheReadTokens: 758515, CacheWriteTokens: 26660,
		OutputTokens: 6283, CostUSD: 0.321241, Billing: "subscription",
	}
	if err := db.RecordUsage(ctx, turn); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	got, err := db.UsageByGroup(ctx, p.ID, "role", time.Time{})
	if err != nil {
		t.Fatalf("UsageByGroup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	g := got[0]
	if g.Key != "coder" {
		t.Errorf("grouped under %q, want coder", g.Key)
	}
	if g.InputTokens != 34 || g.CacheReadTokens != 758515 ||
		g.CacheWriteTokens != 26660 || g.OutputTokens != 6283 {
		t.Errorf("token split lost: in=%d read=%d write=%d out=%d",
			g.InputTokens, g.CacheReadTokens, g.CacheWriteTokens, g.OutputTokens)
	}
	// A subscription turn has real tokens and no marginal dollar cost. Counting
	// it keeps the two separable, so a subscription run does not read as free
	// and a metered one does not read as tokenless.
	if g.SubscriptionTurns != 1 {
		t.Errorf("subscription turns = %d, want 1", g.SubscriptionTurns)
	}
}

func TestUsageGroupsByProviderAndModel(t *testing.T) {
	ctx := context.Background()
	db, p := usageDB(t)

	for _, u := range []UsageTurn{
		{ProjectID: p.ID, Role: "coder", Provider: "anthropic", Model: "sonnet", CostUSD: 1},
		{ProjectID: p.ID, Role: "reviewer", Provider: "anthropic", Model: "opus", CostUSD: 4},
		{ProjectID: p.ID, Role: "reviewer", Provider: "openai", Model: "gpt", CostUSD: 2},
	} {
		if err := db.RecordUsage(ctx, u); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	byProvider, err := db.UsageByGroup(ctx, p.ID, "provider", time.Time{})
	if err != nil {
		t.Fatalf("by provider: %v", err)
	}
	// Ordered by spend, so the expensive provider is the first thing read.
	if len(byProvider) != 2 || byProvider[0].Key != "anthropic" || byProvider[0].CostUSD != 5 {
		t.Errorf("by provider = %+v; want anthropic at 5 first", byProvider)
	}

	byModel, err := db.UsageByGroup(ctx, p.ID, "model", time.Time{})
	if err != nil {
		t.Fatalf("by model: %v", err)
	}
	if len(byModel) != 3 || byModel[0].Key != "opus" {
		t.Errorf("by model = %+v; want opus first", byModel)
	}

	if _, err := db.UsageByGroup(ctx, p.ID, "role; DROP TABLE usage_turns", time.Time{}); err == nil {
		t.Error("an arbitrary grouping was accepted; the column is interpolated into SQL")
	}
}

// Rework costs a full extra lap, and the only way to see it is to total a card
// across every role and every pass rather than per turn.
func TestUsageForTaskSpansEveryLap(t *testing.T) {
	ctx := context.Background()
	db, p := usageDB(t)

	task, err := db.CreateTask(ctx, p.ID, "Calculator", "build it", "coder")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for _, u := range []UsageTurn{
		{ProjectID: p.ID, TaskID: &task.ID, Role: "coder", CostUSD: 1, OutputTokens: 100},
		{ProjectID: p.ID, TaskID: &task.ID, Role: "reviewer", CostUSD: 3, OutputTokens: 50},
		{ProjectID: p.ID, TaskID: &task.ID, Role: "coder", CostUSD: 2, OutputTokens: 80}, // the rework lap
	} {
		if err := db.RecordUsage(ctx, u); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	got, err := db.UsageForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("UsageForTask: %v", err)
	}
	if got.Turns != 3 || got.CostUSD != 6 || got.OutputTokens != 230 {
		t.Errorf("task total = %+v; want 3 turns, $6, 230 output", got)
	}
}

// A cost nobody reported must not be labelled as one somebody calculated.
// Nothing computes cost — model_prices does not exist — so a stored 0.0 that
// claimed to be "computed" would read as a turn that genuinely cost nothing.
func TestUnreportedCostIsUnknownNotComputed(t *testing.T) {
	ctx := context.Background()
	db, p := usageDB(t)

	if err := db.RecordUsage(ctx, UsageTurn{
		ProjectID: p.ID, Role: "coder", OutputTokens: 900, // no CostSource given
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if err := db.RecordUsage(ctx, UsageTurn{
		ProjectID: p.ID, Role: "reviewer", CostUSD: 2, CostSource: CostFromHarness,
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	got, err := db.UsageByGroup(ctx, p.ID, "role", time.Time{})
	if err != nil {
		t.Fatalf("UsageByGroup: %v", err)
	}
	byRole := map[string]UsageTotal{}
	for _, g := range got {
		byRole[g.Key] = g
	}

	if byRole["coder"].UnpricedTurns != 1 {
		t.Error("a turn with no reported cost was counted as priced; the total then reads as complete")
	}
	if byRole["reviewer"].UnpricedTurns != 0 {
		t.Error("a harness-reported cost was counted as unpriced")
	}
	// The tokens are real even when the price is not, and must still be counted.
	if byRole["coder"].OutputTokens != 900 {
		t.Errorf("unpriced turn lost its tokens: %+v", byRole["coder"])
	}
}

// mustDir makes a directory for a project to point at. Paths are validated on
// creation now, so a test cannot name one that does not exist.
func mustDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	return dir
}

// A role's spend is one row, whatever it ran on.
//
// UsageByGroup answers "grouped by one column", which cannot describe a role
// that changed model mid-window: grouping by role loses the models, grouping by
// model splits the role in two, and asking twice gives two answers that can
// disagree about the same turns.
func TestUsageByRoleFoldsModelsIntoOneRow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, repoDir(t, "spend"), "spend", "main")
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateTask(ctx, p.ID, "First", "", "coder")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateTask(ctx, p.ID, "Second", "", "coder")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	turns := []UsageTurn{
		// coder on opus, then moved to sonnet, across two cards.
		{Role: "coder", Model: "opus", Provider: "anthropic", TaskID: &first.ID,
			InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50, CostUSD: 1,
			CostSource: CostFromHarness, Billing: "subscription", At: now.Add(-3 * time.Hour)},
		{Role: "coder", Model: "opus", Provider: "anthropic", TaskID: &second.ID,
			InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50, CostUSD: 1,
			CostSource: CostFromHarness, Billing: "subscription", At: now.Add(-2 * time.Hour)},
		{Role: "coder", Model: "sonnet", Provider: "anthropic", TaskID: &first.ID,
			InputTokens: 50, CacheWriteTokens: 20, OutputTokens: 10, CostUSD: 0.5,
			CostSource: CostUnknown, Billing: "metered", At: now.Add(-time.Hour)},
		// chat spends without a card, which is ordinary and must not vanish.
		{Role: "chat", Model: "sonnet", Provider: "anthropic",
			InputTokens: 10, OutputTokens: 5, CostUSD: 0.25,
			CostSource: CostFromHarness, Billing: "subscription", At: now.Add(-time.Minute)},
	}
	for _, u := range turns {
		u.ProjectID = p.ID
		if err := db.RecordUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.UsageByRole(ctx, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("UsageByRole: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per role", len(got))
	}
	// Most expensive first: the question is where the money went.
	if got[0].Role != "coder" || got[1].Role != "chat" {
		t.Fatalf("rows are %s, %s; want the costliest first", got[0].Role, got[1].Role)
	}

	coder := got[0]
	if coder.Turns != 3 {
		t.Errorf("coder has %d turns, want 3", coder.Turns)
	}
	if coder.CostUSD != 2.5 {
		t.Errorf("coder cost %v, want 2.5", coder.CostUSD)
	}
	if coder.InputTokens != 250 || coder.CacheReadTokens != 1800 ||
		coder.CacheWriteTokens != 20 || coder.OutputTokens != 110 {
		t.Errorf("coder token split is %+v", coder)
	}
	// Both models, busiest first, on one row.
	if len(coder.Models) != 2 || coder.Models[0] != "opus" {
		t.Errorf("coder models are %v, want opus then sonnet", coder.Models)
	}
	if len(coder.Providers) != 1 || coder.Providers[0] != "anthropic" {
		t.Errorf("coder providers are %v", coder.Providers)
	}
	// Two distinct cards, not three — one card was worked on both models.
	if coder.Tasks != 2 {
		t.Errorf("coder touched %d cards, want 2", coder.Tasks)
	}
	if coder.SubscriptionTurns != 2 {
		t.Errorf("%d subscription turns, want 2", coder.SubscriptionTurns)
	}
	if coder.UnpricedTurns != 1 {
		t.Errorf("%d unpriced turns, want 1", coder.UnpricedTurns)
	}
	if coder.LastAt.IsZero() {
		t.Error("coder has no last turn")
	}

	if chat := got[1]; chat.Tasks != 0 {
		t.Errorf("chat is attributed to %d cards; it runs outside the pipeline", chat.Tasks)
	}
}

// The window is the whole point of the range chips.
func TestUsageByRoleRespectsTheWindow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, repoDir(t, "window"), "window", "main")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	for _, at := range []time.Time{now.Add(-48 * time.Hour), now.Add(-time.Minute)} {
		if err := db.RecordUsage(ctx, UsageTurn{
			ProjectID: p.ID, Role: "coder", Model: "opus", Provider: "anthropic",
			OutputTokens: 10, CostUSD: 1, CostSource: CostFromHarness, At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.UsageByRole(ctx, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Turns != 2 {
		t.Errorf("all of history has %d turns, want 2", all[0].Turns)
	}

	recent, err := db.UsageByRole(ctx, p.ID, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Turns != 1 {
		t.Errorf("the last day has %v, want one role with one turn", recent)
	}
}

// "This session" is a real window, and its absence is a different statement
// from "everything".
func TestResolveSpendRange(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, repoDir(t, "ranges"), "ranges", "main")
	if err != nil {
		t.Fatal(err)
	}

	// A project that has never been started has no session, and says so by
	// returning the zero time rather than inventing one.
	at, err := db.ResolveSpendRange(ctx, p.ID, RangeSession)
	if err != nil {
		t.Fatalf("ResolveSpendRange(session): %v", err)
	}
	if !at.IsZero() {
		t.Errorf("a project that never ran reported a session starting at %v", at)
	}

	if _, err := db.StartSession(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	newest, err := db.StartSession(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	at, err = db.ResolveSpendRange(ctx, p.ID, RangeSession)
	if err != nil {
		t.Fatal(err)
	}
	if at.Before(newest.StartedAt.Add(-time.Second)) {
		t.Errorf("session window opens at %v, want the newest session at %v", at, newest.StartedAt)
	}

	if _, err := db.ResolveSpendRange(ctx, p.ID, "37 hours"); err == nil {
		t.Error("an unknown range was accepted")
	}
	if all, err := db.ResolveSpendRange(ctx, p.ID, RangeAll); err != nil || !all.IsZero() {
		t.Errorf("all-time resolved to %v (%v), want the zero time", all, err)
	}
	for _, r := range []string{RangeDay, RangeWeek, RangeMonth} {
		if got, err := db.ResolveSpendRange(ctx, p.ID, r); err != nil || got.IsZero() {
			t.Errorf("%s resolved to %v (%v)", r, got, err)
		}
	}
}
