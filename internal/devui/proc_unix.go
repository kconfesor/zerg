//go:build unix

package devui

import (
	"os/exec"
	"syscall"
	"time"
)

// superviseProcess makes the dev server killable as a whole tree.
//
// The same reason cerebrate does this for agents, for the same reason: killing
// the process we started is not enough. `pnpm exec vite` is a launcher that
// spawns node and exits out of the way, so signalling pnpm leaves the server
// running on a port nobody remembers. Observed exactly that: a stopped daemon
// and a vite still serving from a previous run.
func superviseProcess(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = grace
}

// killGroup ends the process and everything it spawned.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative pid addresses the group. A dev server has nothing to flush, so
	// this does not bother with SIGTERM first: the reason to be gentle with an
	// agent is its buffered output, and there is none here worth a grace
	// period the operator would feel on every Ctrl-C.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
