# zerg

Multi-agent coding orchestrator. A Go daemon supervises a team of agent harnesses working in
isolated git worktrees, routes work between them, and serves a Vue 3 cockpit.

**Everything is configured in the UI.** No config files, no prompt files, no presets to copy. Point
it at a repo, pick your roles, start.

Status: **architecture defined, implementation not started.** See [ARCHITECTURE.md](ARCHITECTURE.md).

## Why

Descended in ideas from [swarm-forge](https://github.com/unclebob/swarm-forge), whose core insight —
agents in separate git worktrees handing each other committed SHAs — is right. Its coordination and
configuration layers are where it hurts. A day of running it produced four hangs that all presented
identically: an agent that looks alive and does nothing. It also silently built a task in the wrong
language, because config is snapshotted into worktrees and a later edit reached no one.

zerg keeps the worktree model and replaces the rest:

- **configure in the UI** — roles, harness, model, prompts are database rows, not files; prompts are
  composed fresh at every spawn, so an edit is live on restart
- **harnesses are adapters**, not a hardcoded switch — `claude` and `pi` first
- **model pickers from the harness's own catalog**, so you stop typing model ids that 400
- **preflight before spawn** — a stale CLI, a corrupt config, an unanswered trust dialog or a broken
  plugin tree becomes a visible blocked role with a stated remedy
- **agents emit structured events**, so the cockpit renders tool calls, tokens and cost instead of
  scraping a terminal
- **leases, not fire-and-forget** — unacked work returns to the queue; a stall is a state, not silence
- **one SQLite database**, one writer, real transactions

Provider setup is out of scope: log into `pi` and `claude` yourself. zerg detects credential state
and tells you what to fix — it never runs a login flow or touches an auth file.

## Defaults

A library of eight role templates ships — planner, coder, reviewer, cleaner, architect, hardener, security, docs. A new project selects `coder` (sonnet) → `reviewer` (opus) and runs. They are ordinary rows: rename
them, replace them, add four more. Nothing is special-cased.

## Layout

```
cmd/zerg/          daemon (zerg up) and agent client (zerg next|done|send|ask)
internal/
  overmind/        orchestrator core
  cerebrate/       per-role agent supervisor
  adapter/         harness contract + claude, pi implementations
  nydus/           message transport: work plane + control plane
  board/           cards and lanes
  store/           sqlite (~/.zerg/zerg.db)
  event/           typed event bus
  api/             http + websocket, serves the embedded cockpit
  hatchery/        workspace and worktree management
web/               Vue 3 + Vite + shadcn-vue cockpit
```

## Stack

Go 1.26.7 · stdlib `net/http` · coder/websocket · creack/pty · modernc.org/sqlite (cgo-free)
Vue 3.5 · Vite 8 · shadcn-vue 2.8 (reka-ui) · Tailwind 4 · @xterm/xterm 6 · TypeScript 6 (pinned)

Pinned versions and their gotchas: [ARCHITECTURE.md §14](ARCHITECTURE.md#14-stack).

## Prerequisites

- **Go 1.26.7+** — not currently installed on this machine (`brew install go`)
- **Node 24.19.0** — pinned in `.nvmrc`; `create-vue` requires `^22.18.0 || >=24.12.0`
- At least one agent harness logged in: `claude` and/or `pi`

No tmux, no babashka, no zsh. Agents are child processes of the daemon, not tmux sessions — see
[ARCHITECTURE.md §7.4](ARCHITECTURE.md#74-no-tmux).

## Build order

Milestones 1–2 are deliberately LLM-free — the coordination layer is testable, and worth testing,
without spending a token.
