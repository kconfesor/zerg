package main

import (
	"context"
	"testing"

	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

// The CLI's old default of 50 looked like an explicit override to the daemon,
// so a planned priority survived direct Send calls but not an agent's handoff.
func TestSendKeepsTaskPriority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority int
		flag     string
		finish   bool
		gate     bool
		reject   bool
		want     int
	}{
		{name: "inherit", priority: 10, want: 10},
		{name: "ordinary default", want: 50},
		{name: "explicit override", priority: 10, flag: "25", want: 25},
		{name: "explicit 50", priority: 10, flag: "50", want: 50},
		{name: "zero inherits", priority: 10, flag: "0", want: 10},
		{name: "completion", priority: 10, finish: true, want: 10},
		{name: "gated completion", priority: 10, finish: true, gate: true, want: 10},
		{name: "completion override", priority: 10, flag: "25", finish: true, want: 25},
		{name: "rejected handoff", priority: 10, flag: "25", gate: true, reject: true, want: 25},
		{name: "rejected completion", priority: 10, finish: true, gate: true, reject: true, want: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAskFixture(t)
			if err := f.db.SelectDefaultTeam(ctx, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if tc.gate {
				coder, err := f.db.GetTemplateByName(ctx, "coder")
				if err != nil {
					t.Fatal(err)
				}
				coder.Gate = store.GateApproval
				if err := f.db.UpdateTemplate(ctx, coder); err != nil {
					t.Fatal(err)
				}
			}
			opts := nydus.NewTaskOpts{
				ProjectID: f.project.ID, Name: "Urgent", Body: "do it", Priority: tc.priority,
			}
			if tc.finish {
				reviewer, err := f.db.GetTemplateByName(ctx, "reviewer")
				if err != nil {
					t.Fatal(err)
				}
				opts.Skip = []string{reviewer.ID}
			}
			n := nydus.New(f.db)
			task, err := n.NewTaskWith(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := n.Claim(ctx, f.project.ID, "coder")
			if err != nil || lease == nil {
				t.Fatalf("claim = %+v, %v", lease, err)
			}
			args := []string{"--task", task.ID, "--commit", "aaaaaaaaaa", "--body", "ready"}
			if !tc.finish {
				args = append(args, "--to", "reviewer")
			}
			if tc.flag != "" {
				args = append(args, "--priority", tc.flag)
			}
			quiet(t, func() error { return runSend(args) })
			quiet(t, func() error { return runDone([]string{"--lease", lease.ID}) })

			var priority int
			if err := f.db.SQL().QueryRowContext(ctx,
				`SELECT priority FROM messages WHERE task_id = ? AND from_role = 'coder'`,
				task.ID).Scan(&priority); err != nil {
				t.Fatal(err)
			}
			if priority != tc.want {
				t.Errorf("stored send priority = %d, want %d", priority, tc.want)
			}
			if tc.reject {
				approvals, err := f.db.ListPendingApprovals(ctx, f.project.ID)
				if err != nil || len(approvals) != 1 {
					t.Fatalf("approvals = %+v, %v", approvals, err)
				}
				if err := n.Reject(ctx, approvals[0].ID, "try again"); err != nil {
					t.Fatal(err)
				}
				lease, err := n.Claim(ctx, f.project.ID, "coder")
				if err != nil || lease == nil || len(lease.Items) != 1 {
					t.Fatalf("rework claim = %+v, %v", lease, err)
				}
				if got := lease.Items[0].Priority; got != task.Priority {
					t.Errorf("rework priority = %d, want the card's %d", got, task.Priority)
				}
			}
		})
	}
}
