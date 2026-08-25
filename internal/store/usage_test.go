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
