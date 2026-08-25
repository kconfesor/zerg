# zerg — Architecture

A multi-agent coding orchestrator. Go core, Vue 3 cockpit, pluggable agent harnesses.

Descended from [swarm-forge](https://github.com/unclebob/swarm-forge) in ideas, not in code. This
document states what is kept, what is replaced, and why — each "why" traces to a concrete failure
mode observed in the predecessor.

---

## 1. Thesis

Three claims drive every decision below.

1. **The harness is an interface, not a branch in a switch statement.** swarm-forge hardcodes
   `#{"claude" "codex" "copilot" "grok"}` in two places (`swarmforge.bb:166`, `:486`); adding a
   backend means editing the launcher. Adapters here are a Go interface with a registry.

2. **Agents should emit events, not paint screens.** swarm-forge infers agent status by grepping the
   tmux pane for a line containing `I'm`, and delivers work by injecting synthetic keystrokes into a
   TUI. Modern harnesses expose structured output (`pi --mode json|rpc`,
   `claude --output-format stream-json`). Consume that instead and the entire scraping layer vanishes.

3. **Failure must be loud at the boundary.** Every incident in a day of running swarm-forge —
   a corrupted global config, a model the CLI was too old to call, a first-run trust dialog, a broken
   extension tree — presented identically: an agent that looked alive and did nothing. All four were
   detectable *before* spawning. Preflight is a first-class subsystem, not an afterthought.

---

## 2. Naming

The StarCraft metaphor is load-bearing for user-facing concepts; code uses plain names where clarity
beats theme.

| Term | Is | Code |
|---|---|---|
| **overmind** | orchestrator daemon; owns all state | `internal/overmind` |
| **cerebrate** | per-role supervisor; owns exactly one agent process | `internal/cerebrate` |
| **nydus** | message transport between roles | `internal/nydus` |
| **hatchery** | a project workspace + its worktrees | `internal/hatchery` |
| **brood** | one named topology (roles + pipeline + prompts) | `brood/<name>/` |
| **larva** | a queued, unassigned task | `board.Task{state:larva}` |

One binary, `zerg`. Subcommands split it into the daemon (`zerg up`) and the agent-facing client
(`zerg next`, `zerg done`, `zerg send`) — see §6.

---

## 3. What is kept from swarm-forge

These were genuinely right and are carried forward:

- **Config-driven topology.** Swarm shape lives in a file, not in code.
- **Git worktree isolation per role.** One repo, one object store, N linked worktrees. Peer commits
  resolve without a fetch. This is the single best idea in the predecessor.
- **Commit-pointer handoffs.** A handoff points at a committed SHA rather than shipping a diff. The
  receiving side merges. Cheap, durable, and git already solved the hard parts.
- **Layered prompts.** A shared constitution plus per-role prompts plus local overrides.
- **Human gates.** Approval before a spec propagates; clarification requests that surface in the UI.
- **A board of cards moving through lanes.** The right mental model for the operator.

## 4. What is replaced, and why

Each row cites the predecessor's failure mode.

| Predecessor mechanism | Failure | Replacement |
|---|---|---|
| Handoff state as files across N worktrees, root-path inferred by git heuristics | Two possible outbox locations with independent sequence counters; filename-keyed dedupe silently **drops** a colliding message while still firing its wake-up | Single SQLite (WAL) database at the hatchery root. One writer, real transactions. Optional read-only file mirror for inspection. |
| `deliver!` = N file copies + N notifies, then one move | Not transactional. Crash mid-loop re-delivers duplicates, re-moves the board, re-notifies. Nothing keys on message `id`. | Outbox pattern in one transaction; delivery is idempotent on `(message_id, recipient)`. |
| Wake-up = `tmux send-keys` of a fixed literal into the session's active pane | Lands in whatever is focused. Hardcoded 150ms/50ms sleeps race the TUI's paste debounce. tmux exit 0 means "keys accepted", never "agent read it". | No push into a TUI. Agent **pulls** via `zerg next` (long-poll) over a unix socket. Delivery is a queue read, not a keystroke. |
| Lost wake-up recovery: none | Agent finishes → peeks empty inbox → prints `NO_TASK` → stops → mail arrives 5ms later → permanent stall, no timer, no retry | Claim/lease model. Work is leased with a deadline; an unacked lease returns to the queue. A stalled role is a visible state, not silence. |
| `PAYLOAD:` runs to EOF, unescaped | Any role can spoof protocol tokens at any other role in 80 chars — `message: NO_TASK` produces a payload line reading exactly `NO_TASK` | JSON envelopes end to end. Protocol tokens cannot appear in user data. |
| Check-then-move selection, no lock | Two concurrent `ready_for_next.sh` runs create two batch dirs and split the queue; every later call then errors `AMBIGUOUS_TASK_STATE` with **no recovery path** | Atomic claim: `UPDATE ... WHERE state='queued'` returning the claimed rows. Concurrency is the database's problem. |
| Helpers resolve the inbox from process cwd | Run from a subdirectory → creates an empty queue there and reports `NO_TASK`. False negative *with* a side effect. | Agent identity comes from a spawn-time token in env. No path inference anywhere. |
| Sender identity = `$SWARMFORGE_ROLE`, unvalidated | Any agent can `export SWARMFORGE_ROLE=architect` and send as the architect | Per-agent capability token minted at spawn, scoped to that role. |
| Terminal role = last line of the config file | Reordering the config silently relocates the end of the pipeline. The other mechanism (exact set-equality broadcast) breaks whenever a utility role is added. | Pipeline declared explicitly, including `terminal`. |
| Board lane moves at *enqueue* time | A card shows "in cleaner's lane" before cleaner has looked at it. Terminal sweep can close unrelated cards. | Lane changes on **ack**, driven by the receiving cerebrate. |
| Batch = every equal-priority item at an instant | Unbounded, unfair. A priority-00 item arriving 1ms late waits behind a 40-item batch. | Batch policy with `max_items` / `max_age`, and priority preemption at claim time. |
| Notes occupy the work queue | One 80-char informational note blocks a role's entire queue until an LLM turn consumes it | Two planes: **work** (tasks) and **control** (notes, answers, cancels). Control never blocks work. |
| Daemon reads socket/roles outside its try block | A deleted socket file terminates the transport *cleanly* — logs "stopped", removes its pid, indistinguishable from normal shutdown. Nothing supervises it. | Supervised components with health endpoints; a dead subsystem is a red banner, not silence. |
| Nothing checks the harness before launching | 40 minutes lost to: triplicated `~/.codex/config.toml`, a CLI too old for its model, an unanswered trust dialog, a broken extension tree | **Preflight** (§5.2). Every one of those is a check that runs before spawn. |

---

## 5. Component model

```
┌──────────────────────────────── zerg (one static binary) ─────────────────────────────────┐
│                                                                                            │
│  ┌─ overmind ────────────────────────────────────────────────────────────────────────┐    │
│  │                                                                                     │    │
│  │   brood config ──▶ pipeline (explicit DAG, terminal declared)                       │    │
│  │                                                                                     │    │
│  │   ┌─ nydus ─────────────┐   ┌─ board ──────────┐   ┌─ store ─────────────────┐     │    │
│  │   │ work plane (leases) │   │ cards & lanes    │   │ SQLite WAL              │     │    │
│  │   │ control plane       │◀─▶│ ack-driven moves │◀─▶│ single writer, txns     │     │    │
│  │   └─────────────────────┘   └──────────────────┘   └─────────────────────────┘     │    │
│  │              ▲                        ▲                                             │    │
│  │              │                        │                                             │    │
│  │   ┌──────────┴────────────────────────┴──────────┐        ┌─ event bus ─────────┐  │    │
│  │   │ cerebrate[specifier]  cerebrate[coder]  ...  │───────▶│ typed, fan-out      │  │    │
│  │   │   ├ preflight                                │        └──────────┬──────────┘  │    │
│  │   │   ├ adapter (claude | pi | …)                │                   │             │    │
│  │   │   ├ structured stdio  ◀── primary            │                   ▼             │    │
│  │   │   └ pty (optional)    ◀── debug attach       │        ┌─ api ───────────────┐  │    │
│  │   └───────────────────────────────────────────────┘        │ net/http + WS      │  │    │
│  │                                                             │ embed.FS → SPA     │  │    │
│  └─────────────────────────────────────────────────────────────┴────────────────────┘    │
│                                          ▲                                                 │
│                    unix socket           │            http/ws                              │
│                          ▲               │               ▲                                 │
└──────────────────────────┼───────────────┴───────────────┼─────────────────────────────────┘
                           │                               │
              ┌────────────┴─────────────┐      ┌──────────┴──────────────┐
              │ agent subprocess         │      │ browser — Vue 3 cockpit │
              │  runs `zerg next/done/   │      │  board, panes, costs    │
              │  send` (same binary)     │      │  attention, chat        │
              └──────────────────────────┘      └─────────────────────────┘
```

### 5.1 cerebrate

One per role. Owns the agent process lifecycle and nothing else.

```go
type Cerebrate struct {
    Role     string
    Adapter  adapter.Adapter
    Worktree string
    Lease    *nydus.Lease   // work currently held
}
```

Responsibilities: preflight → spawn → parse structured output into typed events → publish to the bus
→ track liveness → restart on crash with backoff. It does **not** decide routing; nydus does.

### 5.2 preflight — the headline subsystem

Runs before every spawn. Each check yields `ok` / `blocked(reason, remedy)`. A blocked role appears
in the UI as a blocked role with a stated remedy, never as a silent idle pane.

```go
type Check struct {
    Name   string
    Run    func(ctx context.Context, spec AgentSpec) Result
}
```

Baseline checks, all drawn from real incidents:

| Check | Catches |
|---|---|
| `binary_present` | harness not on PATH |
| `binary_version` | *codex 0.134.0 cannot call `gpt-5.6-sol`* — compare CLI version against model requirement |
| `config_parses` | *triplicated `[features]` key in `~/.codex/config.toml`* — parse the harness's own config before trusting it |
| `auth_valid` | *pi: "No API key found for openai"* — probe credentials for the selected provider |
| `workspace_trusted` | *claude's first-run trust dialog blocking 4 roles* — pre-seed or detect the trust gate |
| `model_available` | model id not in the harness's catalog |
| `plugins_loadable` | *pi's broken extension tree* — a smoke run with the real flags |

`config_parses` and `auth_valid` are per-harness, supplied by the adapter. The rest are generic.

### 5.3 Isolated harness config

swarm-forge launched two codex agents 1.5s apart into fresh directories; both did a non-atomic
read-modify-write of the **global** `~/.codex/config.toml` to register trust, and the writes raced —
producing a file containing three concatenated copies of itself, which then failed to parse for
every codex invocation on the machine, including unrelated projects.

Mitigation: each cerebrate gets a private config directory (`CODEX_HOME`, `PI_CODING_AGENT_DIR`, …)
seeded from the user's real one. Agents never write to shared global state. Adapters declare which
env var relocates their config.

---

## 6. Agent-facing protocol

The agent's whole world is three verbs against a unix socket. Same binary, different subcommand —
no PATH-synced script directory, no `.sh`/`.bb` wrapper pairs, no cwd inference.

```
zerg next  [--wait 30s]   → claim work (long-poll). JSON on stdout.
zerg done  [--result f]   → ack the lease, optionally attach a result
zerg send  --to <role> --type handoff --commit HEAD --task <name>
zerg ask   "<question>"   → raise a clarification to the operator
```

Identity and authorization come from two env vars injected at spawn:
`ZERG_SOCKET`, `ZERG_TOKEN`. The token is role-scoped and per-spawn; `zerg send --from` does not
exist.

### 6.1 Work envelope

```json
{
  "lease_id":  "01JQ...",
  "task":      { "id": "01JQ...", "name": "Calculator" },
  "from":      "specifier",
  "type":      "handoff",
  "commit":    "a1b2c3d4e5",
  "merged":    true,
  "payload":   "…",
  "batch":     [ { "…": "…" } ],
  "expires_at":"2026-08-25T00:31:00Z"
}
```

`merged: true` states that the overmind already performed the merge. swarm-forge merged in the
helper *and* told the agent to merge again in the payload body; it worked only because the second
merge was a no-op. Here the merge happens once, server-side, and the envelope says so.

### 6.2 Leases

A claim is a lease with a deadline, not a file move. Ack (`zerg done`) closes it. Expiry returns the
work to the queue and marks the role degraded. This is the direct answer to *"lost wake-up ⇒
permanent stall, with no timer, no retry, no watchdog."*

### 6.3 Two planes

- **work plane** — tasks and handoffs. Leased, ordered by priority, one unit at a time (or one batch).
- **control plane** — notes, operator answers, cancellations. Delivered out-of-band, never occupies
  a lease, never blocks work.

---

## 7. Pipeline declaration

Terminality is declared, never inferred from file order.

```toml
[brood]
name = "six-pack"

[[role]]
name     = "specifier"
harness  = "claude"
model    = "opus"
worktree = "master"
receive  = "task"
gate     = "approval"          # handoffs from this role wait for a human

[[role]]
name     = "cleaner"
harness  = "pi"
model    = "openai-codex/gpt-5.6-sol"
args     = ["--no-extensions"]
worktree = "cleaner"
receive  = "batch"

  [role.batch]
  max_items = 8
  max_age   = "5m"

[pipeline]
flow     = ["specifier", "coder", "cleaner", "architect", "hardener", "qa"]
terminal = "qa"                 # explicit. moving a role does not move the finish line.
```

---

## 8. Data model

SQLite, WAL, single writer inside the overmind. Sketch:

```sql
tasks     (id, name, lane, state, created_at, updated_at)
messages  (id, from_role, type, priority, task_id, commit_sha, body, created_at)
routes    (message_id, to_role, state, enqueued_at, delivered_at)   -- idempotent per recipient
leases    (id, role, message_id, expires_at, acked_at)
events    (id, ts, role, kind, payload)                             -- append-only, feeds UI + replay
approvals (id, message_id, state, decided_at, decided_by)
runs      (id, role, pid, started_at, exited_at, exit_code, tokens_in, tokens_out, cost_usd)
```

`events` being append-only gives the cockpit free time-travel: the UI is a projection, and a reload
replays rather than re-scrapes.

---

## 9. Cockpit (frontend)

`web/`, built by Vite, embedded into the binary with `//go:embed all:dist` (the `all:` prefix is
required — plain `//go:embed dist` silently skips Vite's `.vite/` manifest directory).

Panels, in priority order:

1. **Attention** — blocked preflights, approvals, clarifications. Anything needing a human.
2. **Board** — one lane per role plus Done. Cards move on ack.
3. **Roles** — per-role health, current lease, live/idle, tokens, cost.
4. **Stream** — typed event feed per role: tool calls, diffs, errors. Not a terminal scrape.
5. **Terminal** *(on demand)* — attach a real pty to a role for debugging, rendered with xterm.js.
6. **Chat** — talk to the master role.

Transport: one WebSocket carrying the typed event stream; REST for commands. The UI subscribes to a
projection, so a browser reload costs a replay, not a rescrape.

---

## 10. Stack

Versions verified against npm dist-tags and `proxy.golang.org` on 2026-08-24.

### Go

| Component | Version | Path | Note |
|---|---|---|---|
| toolchain | **1.26.7** | — | 1.27.0 is 5 days old; pin conservative. 1.27 needs macOS 13+. |
| router | stdlib | `net/http` | Go 1.22+ method+wildcard patterns are enough |
| websocket | v1.8.15 | `github.com/coder/websocket` | successor to `nhooyr.io/websocket`; gorilla is maintenance-only |
| pty | v1.1.24 | `github.com/creack/pty` | tag lags an active HEAD; pseudo-version if an ioctl bug bites |
| sqlite | v1.57.0 | `modernc.org/sqlite` | pure Go — keeps `CGO_ENABLED=0` and the static binary |
| embed | stdlib | `embed` | **must** be `//go:embed all:dist` |

### Frontend

| Component | Version | Note |
|---|---|---|
| Vue | 3.5.41 | 3.6 (Vapor) is RC — do not let `@next` in |
| Vite | 8.2.2 | Rolldown is default, ESM-only, Node 20.19+/22.12+ |
| shadcn-vue | 2.8.2 | CLI; `v3.shadcn-vue.com` is an **archived docs site**, not a package line |
| reka-ui | 2.10.1 | the primitive layer — `radix-vue` was renamed to this and is frozen at 1.9.17 |
| Tailwind | 4.3.3 | CSS-first: no `tailwind.config.js`, use `@tailwindcss/vite` + `@theme {}` |
| @xterm/xterm | 6.0.0 | renamed from `xterm` (deprecated at 5.3.0). v6 **removed the canvas addon** — use WebGL |
| TypeScript | **6.0.3 — pinned** | TS 7 is the Go-native rewrite and ships without a stable programmatic compiler API; `vue-tsc` (Volar) is reported pinned to TS 6 until ~7.1. `vue-tsc`'s peer range says `>=5.0.0`, which is misleading. Verify empirically before unpinning. |
| Pinia | 4.0.3 | |
| vue-router | 5.2.0 | |

> **Node floor:** `create-vue@3.23.0` requires Node `^22.18.0 \|\| >=24.12.0`. This machine's shell
> resolves to v22.12.0 while nvm's default alias is v24.19.0 — scaffolding must run under 24.19.0.
> This is the same version-skew class of bug that broke `pi` locally; pin the Node version in the
> repo (`.nvmrc`) rather than relying on ambient shell state.

---

## 11. Build order

1. `store` + `board` + `nydus` with an in-memory harness stub — prove the protocol without any LLM.
2. `adapter` interface + **claude** adapter (structured mode) + preflight.
3. `cerebrate` supervision, lease expiry, crash/backoff.
4. `api` + minimal cockpit: attention, board, roles.
5. **pi** adapter — the second adapter is what proves the interface is real.
6. pty attach + xterm.js.
7. Cost/token accounting, event replay, time travel.

Milestone 1 is deliberately LLM-free: the coordination layer is where the predecessor's 22 failure
modes lived, and it is testable without spending a token.
