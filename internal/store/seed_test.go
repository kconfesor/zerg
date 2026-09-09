package store

import (
	"context"
	"strings"
	"testing"
)

// Check the text saved in SQLite, not just the constants: installed prompts
// are what agents receive, and runner deliberately bypasses the shared text.
func TestSeededPromptsKeepExecutionBoundaries(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	shared, err := db.GetSetting(ctx, SettingSharedInstructions)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := db.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		prompt := role.Prompt
		if role.Purpose != PurposeRunner {
			prompt = shared + "\n" + prompt
		}
		if !strings.Contains(prompt, NoSubagentsInstruction) {
			t.Errorf("%s can delegate: its effective prompt omits the no-subagents rule", role.Name)
		}
		if role.Purpose == PurposeSupervisor {
			for _, want := range []string{"kind: decide", "kind: plan", "kind: review", "not automatically merged", `--head "<sha>"`} {
				if !strings.Contains(role.Prompt, want) {
					t.Errorf("supervisor prompt omits %q", want)
				}
			}
		}
	}

	for _, want := range []string{"each distinct task", "answered: false", "inspect `git status`", "These envelopes have no work lease"} {
		if !strings.Contains(shared, want) {
			t.Errorf("shared instructions omit %q", want)
		}
	}
	send := strings.Index(shared, "    zerg send ")
	done := strings.Index(shared, `    zerg done --lease "<leaseId>"`)
	if send < 0 || done < send || !strings.Contains(shared, "Only after every send succeeds") {
		t.Error("the protocol must send all results before acknowledging with the required lease id")
	}
}
