//go:build unix

// Package procid answers one question: is the process at this pid still the
// process that was there when we wrote the number down?
//
// A pid on its own cannot be signalled safely. It is a small integer the kernel
// reuses, and a daemon that was SIGKILLed days ago leaves pids behind that now
// belong to whatever the machine has started since. Reproduced against
// zerg.pid: a live `sleep` process's pid put in the file, `zerg down` signalled
// it, killed it, and reported that zerg had stopped.
//
// The fix everywhere it is possible is a kernel-backed lock (see the pid file,
// which holds an flock for the daemon's lifetime). This package is for the case
// where that is not possible: the agents are harness processes zerg spawns and
// does not control the code of, so there is no lock they could hold. What is
// available instead is an identity the operating system already keeps — when
// the process started, and what it is running — which a reused pid cannot
// reproduce.
package procid

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Of returns a token identifying the process at pid, or an error if there is no
// such process or it cannot be identified.
//
// Compare tokens with ==, and treat any error as "cannot verify". Never as
// "not ours": the difference between a pid that is somebody else's and a pid we
// failed to ask about is the difference between leaving a stranger alone and
// leaving one of our own agents running in a worktree.
func Of(pid int) (string, error) {
	if pid <= 1 {
		return "", fmt.Errorf("pid %d is not a process this daemon started", pid)
	}
	// Linux keeps the start time in a file, which is exact, cheap, and needs no
	// subprocess. Field 22 of /proc/<pid>/stat, counted after the closing
	// parenthesis of the command name — the name is unquoted and can contain
	// both spaces and parentheses, so splitting the whole line on spaces is the
	// classic way to read the wrong field.
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(raw, ')'); i >= 0 {
			fields := strings.Fields(string(raw[i+1:]))
			// state is field 3, so starttime (field 22) is index 19 here.
			if len(fields) > 19 {
				return "proc:" + fields[19], nil
			}
		}
		return "", fmt.Errorf("reading the start time of pid %d: /proc/%d/stat is not in the expected form", pid, pid)
	}

	// Everywhere else, ask ps. lstart is the absolute start time rather than an
	// elapsed one, so it does not move between the two calls that have to agree,
	// and the command is in it as well because two processes can start in the
	// same second.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-p", fmt.Sprint(pid), "-o", "lstart=,command=").Output()
	if err != nil {
		return "", fmt.Errorf("identifying pid %d with ps: %w", pid, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("no process with pid %d", pid)
	}
	// Whitespace-normalised: ps pads its columns, and padding that changes with
	// the width of a neighbouring value would make a process fail to match
	// itself.
	return "ps:" + strings.Join(strings.Fields(line), " "), nil
}

// Is reports whether the process at pid is still the one that produced want.
//
// A false answer covers both "a different process has this pid now" and "the
// pid is gone", which are the same thing to every caller here: there is nothing
// of ours to act on. An error is the third case and is not the same as false.
func Is(pid int, want string) (bool, error) {
	if want == "" {
		return false, fmt.Errorf("pid %d was recorded without an identity", pid)
	}
	got, err := Of(pid)
	if err != nil {
		// No such process is an answer, not a failure. ps exits non-zero for a
		// pid that is not there, and so does reading /proc for one.
		if !exists(pid) {
			return false, nil
		}
		return false, err
	}
	return got == want, nil
}

// exists reports whether anything holds this pid.
//
// EPERM is a yes: the process is there and belongs to somebody else, which is
// precisely the case a caller must not read as "gone".
func exists(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// StopGroup asks a whole process group to stop and waits until it has, or until
// grace runs out and it is killed.
//
// The group rather than the leader, for the reason the leader is given one at
// all: every bash tool call an agent makes is a child in it, and a grandchild
// left running is a grandchild still writing to the worktree. SIGTERM first so
// a harness can flush what it was in the middle of writing; SIGKILL only for
// what is still there afterwards.
//
// Returns whether the group was gone by the end. Being unable to signal is not
// an error here: the group exiting between the check and the signal is the
// normal case, and is a success.
func StopGroup(pgid int, grace time.Duration) bool {
	if pgid <= 1 {
		return true
	}
	// Never our own. A pgid read back out of a database is a number, and a
	// number that has come to name the daemon's own group would have the
	// cleanup kill the daemon, its cockpit and every agent it is supervising.
	if pgid == syscall.Getpgrp() || pgid == os.Getpid() {
		return false
	}
	// Negative addresses the group. ESRCH means there was nothing left to ask.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !GroupAlive(pgid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	// A last look, after the kill has had a moment to land. A group still here
	// after SIGKILL is one wedged in the kernel, which is worth reporting
	// rather than papering over.
	for i := 0; i < 20; i++ {
		if !GroupAlive(pgid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !GroupAlive(pgid)
}

// GroupAlive reports whether any process is still in this group.
func GroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
