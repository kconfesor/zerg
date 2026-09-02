package store

import (
	"context"
	"testing"
)

// Forgetting one role's session must leave the rest of the swarm holding
// theirs. The case is a single role whose transcript the harness turns out not
// to have: dropping the whole project's continuity there would cost every other
// agent its memory to fix one agent's, which is the opposite of the trade this
// feature exists to make.
func TestForgettingOneRoleLeavesTheOthersTheirSessions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, repoDir(t, "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	for _, r := range []struct{ role, session string }{
		{"coder", "sess-coder"},
		{"reviewer", "sess-reviewer"},
	} {
		if err := db.SaveRoleSession(ctx, p.ID, r.role, "claude", r.session, "fp"); err != nil {
			t.Fatalf("SaveRoleSession(%s): %v", r.role, err)
		}
	}

	if err := db.ForgetRoleSession(ctx, p.ID, "coder"); err != nil {
		t.Fatalf("ForgetRoleSession: %v", err)
	}

	left, err := db.ListRoleSessions(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListRoleSessions: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("%d sessions left, want only the reviewer's: %+v", len(left), left)
	}
	if left[0].Role != "reviewer" || left[0].SessionID != "sess-reviewer" {
		t.Errorf("left %+v, want the reviewer still holding sess-reviewer", left[0])
	}
}

// Forgetting what was never there is how the recovery path arrives when two
// spawns are refused close together, so it must not be an error.
func TestForgettingARoleWithNoSessionIsNotAnError(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	p, err := db.CreateProject(ctx, repoDir(t, "repo"), "", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.ForgetRoleSession(ctx, p.ID, "coder"); err != nil {
		t.Errorf("ForgetRoleSession on a role with nothing stored: %v", err)
	}
}
