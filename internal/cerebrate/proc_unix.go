//go:build unix

package cerebrate

import (
	"os/exec"
	"syscall"
	"time"
)

// superviseProcess makes a spawned agent killable as a whole tree.
//
// exec.CommandContext kills only the process it started. An agent is a shell
// away from a dozen descendants — every bash tool call is one — and a
// grandchild inherits the stdout pipe. Kill just the parent and that pipe stays
// open, the output reader never sees EOF, and the supervisor waits forever on a
// process that is already gone.
//
// Two mechanisms, because either alone leaves a gap:
//
//   - Setpgid puts the agent in its own process group, and Cancel signals the
//     whole group, so descendants die with it.
//   - WaitDelay bounds the wait anyway. A process can escape its group, and a
//     supervisor that can be wedged by one that does is not a supervisor.
func superviseProcess(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid addresses the group. SIGTERM first: a harness may have
		// buffered output worth flushing, and killing outright would lose the
		// error that explains why it is being stopped.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}

	// After grace, Wait closes the pipes and SIGKILLs whatever is left.
	cmd.WaitDelay = grace
}

// processGroup is the group a spawned agent leads.
//
// Asked of the kernel rather than assumed to be the pid. Setpgid with no Pgid
// makes the two equal today, and a supervisor that writes down a number it will
// later signal should not be the place that assumption is only implied.
func processGroup(pid int) (int, error) { return syscall.Getpgid(pid) }
