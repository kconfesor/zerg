# zerg

Multi-agent coding orchestrator. A Go daemon supervises a team of agent harnesses working in
isolated git worktrees, routes work between them, and serves a Vue 3 cockpit.

**Everything is configured in the UI.** No config files, no prompt files, no presets to copy. Point
it at a repo, pick your roles, start.

Status: **running.** Coordination, harnesses, preflight, board and cockpit are implemented and have
completed real tasks end to end. See [ARCHITECTURE.md](ARCHITECTURE.md).

## Why

Agents in separate git worktrees handing each other committed SHAs is the right shape for this
problem. Coordination and configuration are where it goes wrong, and both fail quietly.

Two incidents set the design. A day of running an earlier build produced four hangs that all
presented identically — an agent that looks alive and does nothing — every one of them detectable
before spawning. And a task was silently built in the wrong language, because config had been
snapshotted into the worktrees and a later edit reached no one.

So:

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
- **nothing reports success it did not observe** — see [§6.1](ARCHITECTURE.md#61-what-the-first-real-run-broke),
  which is the record of a task reaching Done over a branch that had never moved

Provider setup is out of scope: log into `pi` and `claude` yourself. zerg detects credential state
and tells you what to fix — it never runs a login flow or touches an auth file.

## Defaults

A library of eight role templates ships — planner, coder, reviewer, cleaner, architect, hardener,
security, docs. A new project selects `coder` (sonnet) → `reviewer` (opus) and runs. They are
ordinary rows: rename them, replace them, add four more. Nothing is special-cased.

## Layout

```
cmd/zerg/          daemon (zerg up) and agent client (zerg next|done|send|ask)
internal/
  overmind/        orchestrator core: start, stop, supervise, keep work moving
  cerebrate/       per-role agent supervisor
  adapter/         harness contract + claude, pi implementations
  agent/           unix socket serving the four agent verbs
  nydus/           message transport: work plane + control plane
  board/           cards and lanes
  store/           sqlite (~/.zerg/zerg.db), migrations, role library
  event/           typed event bus, logging and usage recording
  preflight/       readiness checks run before anything spawns
  api/             http, serves the embedded cockpit
  hatchery/        workspace and worktree management
web/               Vue 3 + Vite + shadcn-vue cockpit
```

## Stack

Go 1.27 · stdlib `net/http` · modernc.org/sqlite (cgo-free, so `CGO_ENABLED=0` and a static binary)
Vue 3.5 · Vite 8 · shadcn-vue 2.8 (reka-ui) · Tailwind 4 · TypeScript 6 (pinned) · pnpm

`modernc.org/sqlite` is the only non-stdlib Go dependency. Pinned versions and their gotchas:
[ARCHITECTURE.md §14](ARCHITECTURE.md#14-stack).

## Prerequisites

- **Go 1.27+**
- **Node 24.19.0** — pinned in `.nvmrc`; Vite 8 and the shadcn-vue CLI require `^22.18.0 || >=24.12.0`
- **pnpm 11** — the cockpit's package manager (`pnpm-lock.yaml` is the lockfile)
- **git**
- At least one agent harness logged in: `claude` and/or `pi`

No tmux, no babashka, no zsh. Agents are child processes of the daemon — see
[ARCHITECTURE.md §7.4](ARCHITECTURE.md#74-no-tmux).

## Running

```sh
pnpm --dir web install && pnpm --dir web build   # cockpit is embedded in the binary
go build -o zerg ./cmd/zerg
./zerg up                                        # daemon + cockpit on 127.0.0.1:7717
```

Then point it at a repo, pick a team, run preflight, and start.

## Tests

```sh
go test ./internal/...
```

The coordination layer is testable without spending a token, and is tested that way. Tests that
assert an effect check the system that was supposed to change — git, the database — rather than
reading back a field the code set. [§6.1](ARCHITECTURE.md#61-what-the-first-real-run-broke) is what
happens when they don't.
