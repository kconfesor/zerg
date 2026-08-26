# Security

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: **[Security → Report a
vulnerability](https://github.com/kconfesor/zerg/security/advisories/new)**. It opens a private
thread with the maintainers; nothing is public until an advisory is published.

Please do not open a public issue for anything that lets one user reach another user's repository,
credentials or agent processes.

Include what you would want if you were fixing it: the version or commit, what you did, what
happened, and what you expected instead. A failing request or a short script is worth more than a
description of one.

There is no bounty, and no fixed response time, since this is one person's project. Expect a reply
within a week; if a fix is going to take longer than that, the thread will say so rather than going
quiet.

## What zerg is, in security terms

Read this before deciding where to run it. Several of these are deliberate design choices rather
than gaps, and treating them as gaps will lead you to the wrong conclusions.

**zerg runs a daemon that executes code you did not write.** Agents are coding harnesses, `claude`
and `pi`, spawned as child processes with the permissions of the user running the daemon, in git
worktrees inside your repository. They run tests, build tools and whatever else the task needs. The
threat model is the one you already accept by running those CLIs yourself; zerg runs several of
them, unattended, on a schedule you set.

**The HTTP API has no authentication.** None, by design: the intended deployment is a loopback bind
or a [Tailscale](https://tailscale.com) tailnet, where the network is the authentication. The daemon
says so on startup when it binds to anything other than loopback: "reachable at … with no
authentication". Anything that can route to the port can start swarms, read every transcript, and
change what agents are told to do. **Do not expose it to a LAN you do not control, and never to the
public internet.**

What is defended, because it can be reached from a browser that is not yours:

- **Cross-origin writes are refused.** Mutating requests are checked against the daemon's own
  origin, so a page you visit cannot drive a daemon bound to your loopback address.
- **Request bodies are capped**, and read and idle timeouts are bounded, so one client cannot hold
  the daemon open or exhaust it.
- **Agent output is never rendered as HTML.** The Markdown renderer escapes first and builds markup
  only from characters it put there itself, so nothing an agent reads out of a repository can become
  script in the cockpit.
- **A project's files stay inside the project.** Paths served for icons and diffs are resolved and
  checked for containment after symlinks are followed.

**Credentials are never zerg's to hold.** It never runs a login flow, never writes an auth file and
never reads a token out of one. It reports what a harness says about its own credential state and
tells you what to fix. Provider keys live wherever that CLI keeps them.

**What is on disk.** One SQLite database at `~/.zerg/zerg.db`, created 0600 in a 0700 directory,
holding every task, message, transcript and usage row, including whatever your agents wrote into
their handoff notes. It is not encrypted. Back it up like anything else with your work in it.

## Supported versions

`main` only. There are no release branches and no backports; a fix lands on `main` and is in the
next build you make.
