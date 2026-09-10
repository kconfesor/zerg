package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

func TestRunnerPromptForbidsDelegationWithoutAddingThePipelineProtocol(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "runner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTeam(ctx, project.ID, []store.TeamPresetRole{{TemplateID: coder.ID, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{db: db}
	for _, fallback := range []bool{false, true} {
		if fallback {
			runner, err := db.RoleFor(ctx, store.PurposeRunner)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.DeleteTemplate(ctx, runner.ID); err != nil {
				t.Fatal(err)
			}
		}
		_, prompt, err := m.role(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt, store.NoSubagentsInstruction) {
			t.Errorf("fallback=%v: runner did not receive the no-subagents rule", fallback)
		}
		if strings.Contains(prompt, "# Finish a leased task") {
			t.Errorf("fallback=%v: runner received instructions for a lease it cannot hold", fallback)
		}
	}
}
