package runner

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Taking back the ports a preview was using.
//
// Stopping a runner kills the agent, and the comment here used to say the
// server went with it because the agent was its parent. It is not. The agent
// is told to start the server in the background and leave it running, and a
// harness runs its shell commands in a process group of its own: a server left
// over from a stopped preview was found still serving, reparented to init
// (PPID 1) in a group that was never the daemon's. Nothing was going to kill
// it, and it held its port until the machine was rebooted. Every preview takes
// a fresh port, so they accumulated.
//
// So the port is what gets reclaimed, because the port is the thing the daemon
// actually knows. This is deliberately narrow: only ports this daemon handed
// out, only at the moment the session that had them ends, and only when the
// service registered on that port is still recorded as live -- so what is
// killed is a server an agent registered and then abandoned, not whatever
// happens to be listening.

// reclaim stops whatever is still listening on ports a finished session held.
func (m *Manager) reclaim(ports []int) {
	for _, port := range ports {
		for _, pid := range listeners(port) {
			// Asked first. A dev server that flushes and exits is tidier than
			// one that is shot, and most of them do.
			_ = syscall.Kill(pid, syscall.SIGTERM)
			if waitGone(pid, 2*time.Second) {
				continue
			}
			// A shell backgrounding a child sets SIGINT and SIGTERM to be
			// ignored in it, which is how an earlier version of this left a
			// python server running while reporting it stopped.
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// listeners is the pids listening on a port.
//
// lsof rather than reading it out of the kernel: the two ways to do that are
// entirely different on Linux and macOS, this runs once per stopped preview,
// and lsof is present on both. Absent, this returns nothing and the server is
// left alone -- the previous behaviour, which is at least not worse.
func listeners(port int) []int {
	out, err := exec.Command("lsof", "-ti", "tcp:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(line)
		// Never the daemon itself. It cannot be listening on a port it handed
		// to an agent, but a signal sent to pid 0 goes to the whole process
		// group, and that is worth being unable to do by accident.
		if err != nil || pid <= 1 || pid == syscall.Getpid() {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// waitGone reports whether a process ended within the grace period.
func waitGone(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		// Signal 0 tests for existence without delivering anything.
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}
