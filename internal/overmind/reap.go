package overmind

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kconfesor/zerg/internal/procid"
	"github.com/kconfesor/zerg/internal/store"
)

// reapGrace is how long an agent left over from the previous run has to exit
// after being asked, before it is killed.
//
// Longer than stopGrace, which bounds a Stop somebody is waiting on with a
// button. Nobody is waiting on this: it happens once at boot, before the
// listener is announced, and the thing it is waiting for is a harness writing
// out the transcript of work it was in the middle of.
const reapGrace = 15 * time.Second

// ReapPreviousRun stops the agents the last daemon left running, and reports
// what it could not account for.
//
// This is the gap ARCHITECTURE §7.4 used to record and not close. Each agent
// runs in a process group of its own, and the group is signalled from
// cmd.Cancel, which a SIGKILLed daemon never reaches. Measured: after kill -9
// on the daemon, a coder was still running thirty seconds later and still
// writing files into its worktree. Everything the next daemon then did made it
// worse — it reclaimed that agent's lease and handed the card to somebody else,
// and with resumeOnStart it spawned a replacement into the same worktree, with
// the same conversation resumed, while the original was still committing.
//
// Three outcomes per recorded process, and the difference between them is the
// whole point:
//
//   - it is ours and it is there: signal the group, wait for it, forget the row
//   - it is gone, and so is its group: forget the row, nothing to do
//   - it cannot be verified, or the group outlived a leader we can no longer
//     identify: leave the row alone and return it. Signalling on a pid we
//     cannot vouch for is how a cleanup kills a stranger's process, and the row
//     that stays behind is what refuses the automatic resume afterwards.
//
// Returned processes are the third case. The caller decides what to do about
// them; what it must not do is start a swarm in those worktrees unattended.
func ReapPreviousRun(ctx context.Context, db *store.DB, log *slog.Logger) (stopped int, unresolved []store.AgentProcess, err error) {
	rows, err := db.ListAgentProcesses(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, p := range rows {
		mine, err := procid.Is(p.PID, p.Identity)
		switch {
		case err != nil:
			log.Warn("could not tell whether an agent from the previous run is still running",
				"project", p.ProjectID, "role", p.Role, "pid", p.PID, "err", err)
			unresolved = append(unresolved, p)
			continue

		case mine:
			log.Info("stopping an agent left running by the previous daemon",
				"project", p.ProjectID, "role", p.Role, "pid", p.PID,
				"pgid", p.PGID, "worktree", p.Worktree)
			if !procid.StopGroup(p.PGID, reapGrace) {
				log.Error("an agent from the previous run would not stop",
					"project", p.ProjectID, "role", p.Role, "pgid", p.PGID,
					"worktree", p.Worktree)
				unresolved = append(unresolved, p)
				continue
			}
			stopped++

		// The pid is not ours any more, which says nothing about the rest of
		// the group: a harness can exit leaving a bash tool call behind, and
		// that descendant is still in the worktree. A group with nobody in it
		// is genuinely finished. A group with somebody in it and a leader we
		// cannot identify is one we must not signal, because the pid it is
		// named after may now belong to somebody else entirely.
		case procid.GroupAlive(p.PGID):
			log.Warn("a worktree may still be held by a process this daemon cannot identify",
				"project", p.ProjectID, "role", p.Role, "pgid", p.PGID,
				"worktree", p.Worktree)
			unresolved = append(unresolved, p)
			continue
		}

		if err := db.ForgetAgentProcess(ctx, p.ProjectID, p.Role, p.PID); err != nil {
			// The row staying is the safe direction: it refuses a resume for a
			// project whose agents are in fact gone, which costs an operator a
			// click, where forgetting it would let a swarm into a worktree
			// nothing has confirmed is free.
			log.Warn("could not forget an agent process that has stopped",
				"project", p.ProjectID, "role", p.Role, "err", err)
			unresolved = append(unresolved, p)
		}
	}
	return stopped, unresolved, nil
}

// ErrAgentsUnaccountedFor is returned by Start for a project whose previous
// agents could not be stopped or identified.
//
// A 409 rather than a 500 in the API: this is not a fault, it is a machine
// state a person can look at and clear, and the message says how. An operator's
// Stop clears it, because that is the operator saying the project is not
// running — which is the assertion this refusal is waiting for, and which only
// somebody who can look at the machine is in a position to make.
type ErrAgentsUnaccountedFor struct{ Processes []store.AgentProcess }

func (e *ErrAgentsUnaccountedFor) Error() string {
	if len(e.Processes) == 0 {
		return "an agent from a previous run may still be holding this project's worktrees"
	}
	p := e.Processes[0]
	more := ""
	if len(e.Processes) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(e.Processes)-1)
	}
	return fmt.Sprintf("an agent from a previous run may still be holding %s%s: "+
		"role %s was process group %d, which this daemon could neither stop nor identify. "+
		"Check with `ps -o pgid=,command= -g %d`; once nothing is left there, press Stop "+
		"to clear it and start again",
		p.Worktree, more, p.Role, p.PGID, p.PGID)
}
