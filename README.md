# zerg

Multi-agent coding orchestrator. A Go daemon supervises a brood of agent harnesses working in
isolated git worktrees, routes work between them, and serves a Vue 3 cockpit.

Status: **architecture defined, implementation not started.** See [ARCHITECTURE.md](ARCHITECTURE.md).

## Why

Descended in ideas from [swarm-forge](https://github.com/unclebob/swarm-forge), whose core insight —
agents in separate git worktrees handing each other committed SHAs — is right. Its coordination
layer is where it hurts: file-based queues with path-inferred roots, delivery that is not
transactional, and wake-ups delivered as synthetic keystrokes into a TUI. A day of running it
produced four separate hangs, all of which presented as "an agent that looks alive and does nothing".

zerg keeps the worktree model and replaces the coordination layer:

- **harnesses are adapters**, not a hardcoded switch — `claude` and `pi` first
- **agents emit structured events**, so the cockpit renders tool calls, tokens and cost instead of
  scraping a terminal
- **preflight before spawn** — a stale CLI, a corrupt config, an unanswered trust dialog or a broken
  plugin tree becomes a visible blocked role with a stated remedy
- **leases, not fire-and-forget** — unacked work returns to the queue; a stall is a state, not silence
- **one SQLite database**, one writer, real transactions — no dedupe keyed on filenames

## Layout

```
cmd/zerg/          daemon (zerg up) and agent client (zerg next|done|send|ask)
internal/
  overmind/        orchestrator core
  cerebrate/       per-role agent supervisor
  adapter/         harness contract + claude, pi implementations
  nydus/           message transport: work plane + control plane
  board/           cards and lanes
  store/           sqlite
  event/           typed event bus
  api/             http + websocket, serves the embedded cockpit
  hatchery/        workspace and worktree management
web/               Vue 3 + Vite + shadcn-vue cockpit
brood/             example topologies
```

## Stack

Go 1.26.7 · stdlib `net/http` · coder/websocket · creack/pty · modernc.org/sqlite (cgo-free)
Vue 3.5 · Vite 8 · shadcn-vue 2.8 (reka-ui) · Tailwind 4 · @xterm/xterm 6 · TypeScript 6 (pinned)

Pinned versions and their gotchas are documented in [ARCHITECTURE.md §10](ARCHITECTURE.md#10-stack).

## Build order

Milestone 1 is deliberately LLM-free — the coordination layer is testable, and worth testing,
without spending a token.
