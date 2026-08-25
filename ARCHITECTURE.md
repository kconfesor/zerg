# zerg — Architecture

A multi-agent coding orchestrator. Go core, Vue 3 cockpit, pluggable agent harnesses.

**Everything is configured in the UI.** There are no config files, no prompt files, no presets to
copy. You point it at a repo, pick your roles, and start.

Descended from [swarm-forge](https://github.com/unclebob/swarm-forge) in ideas, not in code. Each
"why" below traces to a concrete failure mode observed in the predecessor.

---

## 1. Thesis

1. **The harness is an interface, not a branch in a switch statement.** swarm-forge hardcodes
   `#{"claude" "codex" "copilot" "grok"}` in two places; adding a backend means editing the launcher.
   Adapters here are a Go interface with a registry.

2. **Agents emit events; they do not paint screens.** swarm-forge infers status by grepping a tmux
   pane for a line containing `I'm`, and delivers work by injecting keystrokes into a TUI. Modern
   harnesses expose structured output (`pi --mode json|rpc`, `claude --output-format stream-json`).
   Consume that and the entire scraping layer disappears.

3. **Configuration is a database, not a filesystem.** swarm-forge spreads config across
   `swarmforge.conf`, `constitution.prompt`, `constitution/articles/*.prompt` and
   `roles/<role>.prompt` — then copies *snapshots* of them into every worktree. Editing the original
   after launch changes nothing, silently. (Verified the hard way: a `project.prompt` edit setting the
   language to Rust never reached a single agent, because worktrees were cut from a commit made
   before the edit. Six agents built the task in Clojure instead, with no warning.) One database,
   one source of truth, composed fresh at every spawn.

4. **Failure must be loud at the boundary.** Every incident in a day of running swarm-forge — a
   corrupted global config, a CLI too old for its model, an unanswered trust dialog, a broken plugin
   tree — presented identically: an agent that looked alive and did nothing. All four were detectable
   *before* spawning.

---

## 2. Scope boundary: provider setup

zerg **does not** manage provider credentials. It never runs a login flow, stores an API key, or
edits a harness's auth file. Users log into `pi`, `claude`, etc. themselves, using those tools.

zerg **does** detect credential state and say so plainly. `pi: no credentials for provider 'openai'
— run /login in pi` is a blocked role with a remedy, not a silent hang. Detection is in scope;
setup is not.

---

## 3. Naming

| Term | Is | Code |
|---|---|---|
| **overmind** | orchestrator daemon; owns all state | `internal/overmind` |
| **cerebrate** | per-role supervisor; owns exactly one agent process | `internal/cerebrate` |
| **nydus** | message transport between roles | `internal/nydus` |
| **hatchery** | a project workspace + its worktrees | `internal/hatchery` |
| **larva** | a queued, unassigned task | `board.Task{state:larva}` |

One binary, `zerg`. `zerg up` runs the daemon and opens the cockpit; `zerg next|done|send|ask` is
the agent-facing client (§7).

---

## 4. Configuration model

This is the part that most differs from the predecessor.

### 4.1 Library, team, runtime

Three layers, and the separation is what makes "configure once" and "every project is different"
both true at the same time.

**Role library** — global, in `~/.zerg/zerg.db`. A catalog of role *templates*: what a planner is,
what a reviewer is, what prompt each carries. Ships with a set of built-ins (§4.5); you edit them and
add your own. Editing a template is editing the idea of that role everywhere.

**Project team** — per project. You pick which roles from the library this project uses, drag them
into order, and optionally override a field or two for this repo alone. A Rust service and a docs
site want different teams from the same library.

**Runtime** — per project: tasks, messages, leases, events, cost. On disk a project holds only git
artifacts, `<repo>/.worktrees/<role>`.

The middle layer is what earlier drafts were missing. Global-only roles meant every project shared
one pipeline; per-project-only roles meant rewriting the same prompt in every repo. Selection makes
the override a natural field on the join rather than a patch bolted onto the side.

### 4.2 A role template, in full

Everything below is a form field in the UI. There is no other way to set any of it, by design.

| Field | UI control | Notes |
|---|---|---|
| name | text | also names the worktree: `.worktrees/<name>` |
| harness | select | populated from the adapter registry — `claude`, `pi` |
| model | combobox | options fetched live from the harness (§4.3); free text accepted |
| args | tag input | extra CLI flags, e.g. `--no-extensions` |
| receive | select | `task` (one at a time) or `batch` |
| batch policy | number + duration | `max_items`, `max_age` — only when receive is `batch` |
| prompt | editor | this role's instructions |
| gate | select | `none` or `approval` — hold this role's handoffs for a human |

When a template is added to a project, three more fields exist on that project only: **position**
(drag to order), **enabled**, and **overrides** for model and args. Overriding is explicit and
visible — a role showing an override is badged in the team list, so a project that quietly drifted
from the library is legible rather than mysterious.

Plus one **shared instructions** document, global, applied to every role. That single editable
document replaces swarm-forge's `constitution.prompt` + `constitution/articles/*.prompt` layering.

### 4.3 Model discovery

Typing model ids by hand is how you get `Model metadata for 'gpt-5.6-sol' not found` and
`The 'gpt-5.6-sol' model requires a newer version of Codex` — twenty minutes of an agent looking
alive while every turn 400s.

So `Adapter.ListModels()` asks the harness what it can actually run (`pi --list-models`, claude's
alias set) and the UI renders a picker. The field still accepts free text, because a harness catalog
can lag a working model — `gpt-5.6-sol` is absent from pi's catalog and runs fine. Free text gets a
warning, not a block.

### 4.4 Prompt composition

At **every spawn**, the overmind composes `shared instructions + role prompt` from the database into
a temp file and hands it to the adapter. Nothing is copied into a worktree; nothing persists between
runs.

Consequence: edit a prompt in the UI, restart the role, and the change is live. This is the direct
fix for the snapshot staleness that silently produced a Clojure calculator when the config said Rust.

### 4.5 The built-in library

Eight templates ship, covering every shape swarm-forge split across `two-pack`, `four-pack` and
`six-pack` — except here they are rows in a picker, not branches of the orchestrator you have to
check out.

| Template | Model | Receive | Gate | Does |
|---|---|---|---|---|
| `planner` | opus | task | **approval** | turns intent into a written spec, then waits for a human |
| `coder` | sonnet | task | — | implements the spec, writes tests, commits |
| `reviewer` | opus | batch | — | reviews the change against the spec, runs tests, reports or hands back |
| `cleaner` | sonnet | batch | — | behavior-preserving cleanup, duplication, dead code |
| `architect` | opus | batch | — | module boundaries, dependency direction, structural drift |
| `hardener` | sonnet | batch | — | edge cases, error paths, mutation-style probing |
| `security` | opus | batch | — | input handling, secrets, dependency and injection review |
| `docs` | sonnet | batch | — | README, API docs, changelog |

Reviewing roles run the stronger model on purpose: catching a wrong change is harder than making a
plausible one.

**A new project starts with `coder` → `reviewer` selected** — enough to be useful in two clicks.
Everything else is one checkbox away, and none of it is special-cased: a built-in is an ordinary row
you can edit, duplicate, or delete.

### 4.6 The planner and the approval gate

`planner` is the answer to "write the spec, then let me approve it before anything happens", and it
needs no new machinery — it is a template with `gate: approval`, a field roles already have.

The flow: planner writes the spec and commits it, then queues its handoff downstream. The gate holds
that handoff. **Attention** shows the task with a link to the spec itself — a `file` artifact
(§13) rendered inline, not a filename you go hunting for — with **Approve** and **Reject**.
Approving delivers the handoff and moves the card; rejecting returns it to the planner with your note
attached, and nothing downstream ever saw it.

The gate is a field, not a role, so it composes: put `approval` on `architect` when a project needs
structural changes signed off, or on nothing at all for a repo you are happy to let run unattended.

---

## 5. What is kept from swarm-forge

- **Git worktree isolation per role.** One repo, one object store, N linked worktrees; peer commits
  resolve without a fetch. The single best idea in the predecessor.
- **Commit-pointer handoffs.** A handoff points at a SHA; the receiver merges. Git already solved the
  hard parts.
- **Human gates** — approvals and clarification requests surfaced in the UI.
- **A board of cards moving through lanes.** The right mental model for an operator.

One change: swarm-forge required exactly one role to occupy the repo root (`master`), which made that
role special in the config, in routing, and in the board. Here **every role gets a worktree** and the
repo root is the integration branch. When the terminal role completes, the *overmind* merges to the
base branch. Integration belongs to the orchestrator, not to whichever agent happened to be last.

## 6. What is replaced, and why

| Predecessor mechanism | Failure | Replacement |
|---|---|---|
| Config as files, snapshotted into each worktree | Post-launch edits silently invisible to every agent; a Rust config produced a Clojure implementation | Database is the only source of truth; prompts composed fresh at spawn |
| Topology fixed by `swarmforge.conf` + preset branches (`two-pack`, `four-pack`, `six-pack`) | Changing the team means checking out a different git branch of the orchestrator | Roles are rows, edited in the UI; the team *is* the config |
| Handoff state as files across N worktrees, root path inferred by git heuristics | Two possible outbox locations with independent sequence counters; filename-keyed dedupe silently **drops** a colliding message while still firing its wake-up | Single SQLite (WAL) database, one writer, real transactions |
| `deliver!` = N file copies + N notifies, then one move | Not transactional. Crash mid-loop re-delivers duplicates and re-moves the board. Nothing keys on message `id` | Outbox pattern in one transaction; idempotent on `(message_id, recipient)` |
| Wake-up = `tmux send-keys` of a fixed literal into the session's active pane | Lands in whatever is focused; hardcoded 150ms/50ms sleeps race the TUI's paste debounce; tmux exit 0 means "keys accepted", never "agent read it" | Agent **pulls** via `zerg next` (long-poll) over a unix socket |
| Lost wake-up recovery: none | Agent finishes → peeks empty inbox → prints `NO_TASK` → stops → mail arrives 5ms later → permanent stall, no timer, no retry | Leases with deadlines; unacked work returns to the queue and the role shows degraded |
| `PAYLOAD:` runs to EOF, unescaped | Any role can spoof protocol tokens at any other role in 80 chars — `message: NO_TASK` yields a payload line reading exactly `NO_TASK` | JSON envelopes end to end |
| Check-then-move selection, no lock | Two concurrent claims create two batch dirs and split the queue; every later call errors `AMBIGUOUS_TASK_STATE` with **no recovery path** | Atomic claim: `UPDATE ... WHERE state='queued'` returning claimed rows |
| Helpers resolve the inbox from process cwd | Run from a subdirectory → creates an empty queue there and reports `NO_TASK`. False negative *with* a side effect | Identity from a spawn-time token in env; no path inference anywhere |
| Sender identity = `$SWARMFORGE_ROLE`, unvalidated | Any agent can `export SWARMFORGE_ROLE=architect` and send as the architect | Per-agent capability token minted at spawn |
| Terminal role = last line of the config file | Reordering the file silently relocates the end of the pipeline | Last enabled role in the UI ordering, shown as a `terminal` badge |
| Board lane moves at *enqueue* time | A card shows "in cleaner's lane" before cleaner has looked at it | Lane changes on **ack** |
| Batch = every equal-priority item at an instant | Unbounded and unfair; a priority-00 item arriving 1ms late waits behind a 40-item batch | Batch policy (`max_items`, `max_age`) set per role in the UI; priority preemption at claim time |
| Notes occupy the work queue | One 80-char informational note blocks a role's queue until an LLM turn consumes it | Two planes: work and control (§7.3) |
| Daemon reads socket/roles outside its try block | A deleted socket file terminates the transport *cleanly* — logs "stopped", removes its pid, indistinguishable from normal shutdown | Supervised components with health endpoints |
| Nothing checks the harness before launching | 40 minutes lost to a triplicated `~/.codex/config.toml`, a CLI too old for its model, an unanswered trust dialog, a broken extension tree | **Preflight** (§8) |

---

## 7. Component model

```
┌──────────────────────────── zerg (one static binary) ─────────────────────────────┐
│                                                                                    │
│  ┌─ overmind ──────────────────────────────────────────────────────────────────┐  │
│  │                                                                              │  │
│  │  ┌─ config ────────────┐   ┌─ nydus ─────────────┐   ┌─ board ───────────┐  │  │
│  │  │ roles (UI CRUD)     │   │ work plane (leases) │   │ cards & lanes     │  │  │
│  │  │ shared instructions │──▶│ control plane       │◀─▶│ ack-driven moves  │  │  │
│  │  │ projects            │   └─────────────────────┘   └───────────────────┘  │  │
│  │  └─────────┬───────────┘              ▲                       ▲             │  │
│  │            │                          │                       │             │  │
│  │            ▼            ┌─────────────┴───────────────────────┴──────────┐  │  │
│  │  ┌─ store ───────────┐  │ cerebrate[coder]  cerebrate[reviewer] ...         │  │  │
│  │  │ SQLite WAL        │  │   ├ preflight                                  │  │  │
│  │  │ ~/.zerg/zerg.db   │  │   ├ adapter (claude | pi | …)                   │  │  │
│  │  │ single writer     │  │   ├ structured stdio ◀── primary                │  │  │
│  │  └───────────────────┘  │   └ pty              ◀── debug attach           │  │  │
│  │                         └───────────────────┬────────────────────────────┘  │  │
│  │                                             ▼                               │  │
│  │  ┌─ event bus ─────────┐        ┌─ api ─────────────────┐                   │  │
│  │  │ typed, fan-out      │───────▶│ net/http + WS         │                   │  │
│  │  └─────────────────────┘        │ embed.FS → Vue SPA    │                   │  │
│  └──────────────────────────────────┴───────────────────────┴──────────────────┘  │
│                        ▲                              ▲                            │
│           unix socket  │                    http/ws   │                            │
└────────────────────────┼──────────────────────────────┼────────────────────────────┘
                         │                              │
          ┌──────────────┴───────────┐      ┌───────────┴─────────────┐
          │ agent subprocess         │      │ browser — Vue 3 cockpit │
          │  runs `zerg next|done|   │      │  configure · observe    │
          │  send` (same binary)     │      │                         │
          └──────────────────────────┘      └─────────────────────────┘
```

### 7.1 cerebrate

One per enabled role. Owns the agent process lifecycle and nothing else: preflight → spawn → parse
structured output into typed events → publish → track liveness → restart with backoff. It does not
decide routing; nydus does.

### 7.2 Agent-facing protocol

The agent's whole world is four verbs against a unix socket. Same binary, different subcommand — no
PATH-synced script directory, no `.sh`/`.bb` wrapper pairs, no cwd inference.

```
zerg next [--wait 30s]   claim work (long-poll); JSON on stdout
zerg done [--result f]   ack the lease
zerg send --to <role> --commit HEAD --task <name>
zerg ask  "<question>"   raise a clarification to the operator
```

Identity arrives as `ZERG_SOCKET` and `ZERG_TOKEN`, injected at spawn. The token is role-scoped and
per-spawn; there is no `--from` flag to forge.

Work envelope:

```json
{
  "lease_id": "01JQ…", "task": {"id": "01JQ…", "name": "Calculator"},
  "from": "coder", "type": "handoff", "commit": "a1b2c3d4e5",
  "merged": true, "payload": "…", "expires_at": "2026-08-25T00:31:00Z"
}
```

`merged: true` states the overmind already merged. swarm-forge merged in the helper *and* told the
agent to merge again in the payload; it worked only because the second merge was a no-op.

**Leases.** A claim has a deadline. Ack closes it; expiry returns the work to the queue and marks the
role degraded. This is the answer to "lost wake-up ⇒ permanent stall, no timer, no retry".

### 7.3 Two planes

- **work** — tasks and handoffs. Leased, priority-ordered, one unit (or one batch) at a time.
- **control** — notes, operator answers, cancellations. Out-of-band, never occupies a lease, never
  blocks work.

### 7.4 No tmux

swarm-forge is built on tmux: one session per role, a project-scoped socket, `send-keys` for
delivery, `capture-pane` for the UI. zerg uses none of it. Agents are ordinary child processes of the
daemon, supervised with `os/exec`.

Every job tmux was doing has a better owner once agents emit structured events:

| tmux was providing | Now |
|---|---|
| process supervision | `os/exec` + cerebrate. Real exit codes and signals, restart with backoff — instead of a session that stays "alive" around a process returning HTTP 400 on every turn |
| somewhere for the TUI to live | Nothing to host in structured mode. Takeover allocates a pty directly (`creack/pty`); tmux was never needed for that |
| the delivery channel (`send-keys`) | Structured input over a pipe (§10.2) |
| the observation channel (`capture-pane`) | The typed event stream (§10.1) |
| per-project isolation via a socket path | The daemon owns every child; there is no shared namespace to collide in |
| **surviving the operator's terminal closing** | See below — the one job that still needs an answer |

That last row is the only real loss, and tmux's version of it was weaker than it looked: it keeps a
*process* alive, which does nothing when the orchestrator has lost its *state*. swarm-forge's daemon
terminated cleanly on a missing socket file — logging "stopped", removing its pid, indistinguishable
from a normal shutdown — while agents sat there alive and idle and mail piled up in outboxes with no
error surfaced anywhere.

zerg answers it in two pieces:

- **`zerg up --detach`** runs the daemon detached from the invoking shell, with a launchd/systemd
  unit for machines that want it always-on. Closing a terminal is not an event the system notices.
- **Restart is a first-class path, not a recovery hack.** If the daemon does die, its children die
  with it; on restart, leases have expired, so claimed-but-unacked work is already back in the queue.
  Roles respawn and resume their harness session (`claude --resume`, `pi --session`), so context
  survives even though the process did not. Nothing has to be reattached, and nothing is silently
  half-delivered.

The prerequisite list shrinks accordingly: Go and a logged-in harness. No tmux, no babashka, no zsh.

### 7.5 Transports

Four channels, each carrying what it is good at. "One WebSocket for everything" is a common instinct
and a bad one — it reinvents request/response correlation, status codes, caching and range requests,
all of which HTTP already has.

| Channel | Transport | Carries |
|---|---|---|
| agent ↔ overmind | **unix socket** | `zerg next/done/send/ask`; never leaves the machine |
| cockpit → overmind | **HTTP/REST** | commands: create task, edit role, approve, start, stop |
| overmind → cockpit | **WebSocket** | the live typed event stream, and pty bytes during takeover |
| artifact bytes | **plain HTTP** | files, images, downloads — see §13 |

Commands are request/response with a status code and a body, so they belong on REST: a rejected
approval is a `409`, not a hand-rolled correlation id and an error frame invented for the occasion.

The WebSocket is multiplexed by channel id, so events and a takeover pty share one connection and one
auth path. On connect the client sends the last event id it saw and the server replays forward from
`events` — the same mechanism that makes a browser reload cost a replay rather than a rescrape
(§10.1). SSE would serve the event half equally well, and its built-in `Last-Event-ID` maps neatly
onto that table; WebSocket wins only because takeover needs bidirectional bytes anyway, and one
mechanism beats two.

Runs before every spawn. Each check yields `ok` or `blocked(reason, remedy)`. A blocked role renders
in **Attention** with both — never as an idle pane that happens to be doing nothing.

| Check | Catches | Source |
|---|---|---|
| `binary_present` | harness not on PATH | generic |
| `binary_version` | *codex 0.134.0 cannot call `gpt-5.6-sol`* | adapter |
| `config_parses` | *triplicated `[features]` key in `~/.codex/config.toml`* | adapter |
| `auth_valid` | *pi: "No API key found for openai"* → "log in with pi" (detect only, §2) | adapter |
| `workspace_trusted` | *claude's first-run trust dialog blocking four roles* | adapter |
| `model_available` | model id absent from the harness catalog → warn, don't block | adapter |
| `plugins_loadable` | *pi's broken extension tree* — smoke run with the real flags | adapter |

### 8.1 Two moments, one check suite

The same checks run at two points, because the two failures they prevent are different.

**Project setup — the readiness gate.** Adding a project, or pressing Start, runs the full suite
across **every enabled role** first, in parallel, and renders a readiness panel: one row per role,
each check green, amber or red, with the remedy inline for anything failing. Start is disabled while
any role is red.

This is the moment that matters. Half our lost day came from a swarm that launched *successfully* —
six sessions up, dashboard green, board drawn — while four roles sat at a trust dialog and two more
were dead on a config parse error. Nothing was wrong with the launch; everything was wrong with the
agents. A team that cannot work should never reach a running board.

Red is blocking. Amber (an unlisted model, a harness whose version could not be determined) shows a
warning and allows Start with an explicit acknowledgement, since a catalog can lag a model that
works. The panel is re-runnable on demand — you fix a login in another terminal, hit **Re-check**,
and watch the row go green without restarting anything.

**Spawn — the guard.** The same suite runs again before each individual spawn, because state drifts
between setup and launch and between one task and the next: a token expires, a `brew upgrade`
replaces a binary, another tool rewrites a shared config. A role that fails here does not spawn; it
appears in Attention as blocked with its remedy, and the work it would have claimed stays queued
rather than vanishing into a lease held by a process that cannot run.

Checks are cheap (a version probe, a config parse, a credential read) and cached briefly per role,
so the spawn guard costs milliseconds.

### 8.2 Isolated harness config

swarm-forge launched two codex agents 1.5s apart into fresh directories; both did a non-atomic
read-modify-write of the **global** `~/.codex/config.toml` to register trust. The writes raced,
producing a file containing three concatenated copies of itself, which then failed to parse for every
codex invocation on the machine — including unrelated projects.

Each cerebrate therefore gets a private harness config directory (`CODEX_HOME`,
`PI_CODING_AGENT_DIR`, …) seeded from the user's real one. Agents never write shared global state.
Adapters declare which env var relocates their config.

---

## 9. Data model

SQLite, WAL, single writer inside the overmind.

```sql
-- global library, edited exclusively through the UI
role_templates (id, name, harness, model, args, receive,
                batch_max_items, batch_max_age, prompt, gate,
                builtin, created_at, updated_at)
settings       (key, value)       -- shared instructions, preferences
projects       (id, path, name, base_branch, last_opened_at)

-- which templates this project uses, in what order, with what overrides
project_roles  (project_id, template_id, position, enabled,
                model_override, args_override)

-- per-project runtime
sessions    (id, project_id, started_at, ended_at, end_reason)

tasks       (id, project_id, session_id, name, lane, state,
             created_at, first_claimed_at, completed_at,
             active_ms)          -- summed lease time; wall time is completed−created
messages    (id, project_id, from_role, type, priority, task_id, commit_sha, body, created_at)
routes      (message_id, to_role, state, enqueued_at, delivered_at)   -- idempotent per recipient
leases      (id, project_id, role, message_id, expires_at, acked_at)
events      (id, project_id, ts, role, kind, payload)                 -- append-only
runs        (id, project_id, role, pid, started_at, exited_at, exit_code)
approvals   (id, project_id, message_id, state, decided_at)

-- one row per model turn, not per run: this is what the cost dashboard reads
usage_turns (id, project_id, task_id, role, run_id, ts,
             harness, provider, model,
             input_tokens,        -- uncached input only
             cache_write_tokens,  -- billed ~1.25x (5m TTL) or 2x (1h)
             cache_read_tokens,   -- billed ~0.1x
             output_tokens,
             cost_usd,
             cost_source,         -- 'harness' | 'computed'
             billing)             -- 'metered' | 'subscription'

-- prices carry effective dates; introductory rates expire
model_prices (provider, model, effective_from, effective_to,
              input_per_mtok, output_per_mtok,
              cache_write_mult, cache_read_mult)

-- pre-aggregated history; small, permanent, powers every long-range chart
daily_rollup (project_id, day, role, provider, model,
              input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
              cost_usd, turns, active_ms, tasks_completed)
```

`events` being append-only gives the cockpit free time travel: the UI is a projection, so a reload
replays rather than re-scrapes.

---

## 10. Cockpit

`web/`, built by Vite, embedded with `//go:embed all:dist` — the `all:` prefix is required, since
plain `//go:embed dist` silently skips Vite's `.vite/` manifest directory.

**Configure**

- **Projects** — list, add by directory picker, set base branch, open. Two clicks to a running swarm.
- **Readiness** — the preflight panel (§8.1). One row per enabled role, every check with its status
  and an inline remedy, a **Re-check** button, and a **Start** that stays disabled until the team can
  actually work.
- **Team** — two columns. Left, the **library**: every template with a checkbox. Right, **this
  project's pipeline**: the selected roles, drag to reorder, last enabled one wearing a `terminal`
  badge, any overridden role badged as such. Checking a box adds a role; dragging orders it.
- **Role editor** — every field in §4.2. Harness select, model combobox populated from the live
  harness catalog, prompt editor, batch policy, gate. Editing a **template** changes that role
  everywhere; editing a project's copy sets an **override** and says so. Saving a field on a running
  role restarts just that cerebrate.
- **Shared instructions** — one editor, applies to all roles.

**Observe**

- **Attention** — blocked preflights (with remedies), approvals, clarifications. Anything needing a
  human, first.
- **Board** — one lane per enabled role plus Done. Cards move on ack.
- **Roles** — per-role health, current lease, live/idle, tokens, cost.
- **Spend** — the cost dashboard (§11.4). A summary strip carries session tokens, dollars and cache
  rate; every figure on it is a filter. Click a provider chip to scope the page to Anthropic or
  OpenAI; click a role to scope to that stage. Breakdowns by **provider**, by **role/stage**, and by
  **task**, each showing the three-way input split rather than one input number, with subscription
  rows labelled as estimates. Drill-down runs summary → provider → role → task → turn, ending at the
  individual turn that the activity view already shows.
- **History** — the long view (§12), scoped per project: spend over time stacked by role, cost per
  task ranked, wall time against active time, cache rate as a line, and a session log. Reads
  `daily_rollup`, so a twelve-month range is as fast as a one-day range.
- **Chat** — talk to the first role in the pipeline.

Transport: one WebSocket carrying typed events; REST for commands.

### 10.1 Watching an agent work

An agent in structured mode is not painting a screen, so there is no TUI to attach to — a pty on
that process shows JSON lines scrolling past. Three modes cover what a terminal was being used for,
and the first is better than the thing it replaces.

**Activity view** *(default)*. Rendered from the structured stream: every tool call, every `bash`
command with its stdout and exit code, every file edit as a diff, reasoning, errors. This is the
"what is it doing right now" view. Because it is structured rather than scraped, it is searchable,
filterable by role or tool, linkable per event, and replayable from the `events` table after a
reload. A terminal scrape offers none of that, and swarm-forge's version of this question was
grepping a pane for a line containing `I'm`.

**Raw stream**. The JSON lines as received. For debugging an adapter, not for watching work.

**Interactive takeover** *(on demand)*. Sometimes you genuinely want the harness's own TUI — to run
its slash commands, or to drive it by hand. That is a deliberate mode switch, not a second view of
the same process: the cerebrate stops the headless process and relaunches that one role in its
native TUI on a pty, which the cockpit renders with xterm.js. Structured events pause for the
duration and the role is marked `takeover` on the board, because the orchestrator can no longer see
what it is doing. Detaching relaunches it headless.

### 10.2 Talking to a running agent

Both target harnesses accept **streaming structured input** alongside streaming output
(`claude --input-format stream-json`, `pi --mode rpc`). Chat messages, clarification answers and
follow-ups are therefore delivered as structured messages to a running agent.

No keystrokes are ever injected. swarm-forge's wake-up was `tmux send-keys` of a fixed literal into
whichever pane happened to be focused, with hardcoded 150ms/50ms sleeps racing the TUI's paste
debounce — and a tmux exit code of 0 meant "keys accepted", never "the agent read it". Delivery here
is a write to a pipe with a response event to confirm it landed.

---

## 11. Token economics

zerg does not call the Messages API; the harnesses do. What zerg controls is the *bytes it hands
them* and *how often it restarts them* — and both decide whether prompt caching works.

### 11.1 Output format costs nothing

`--output-format stream-json` is a serialization choice for the CLI's stdout. It changes how the
harness prints what it already received; the request and the completion are identical. Structured
mode is not more expensive than a TUI.

It is slightly *cheaper*, for one reason. swarm-forge's dashboard reads agent status by grepping the
pane for a line containing `I'm`, so its constitution instructs every agent to narrate status in
prose. Those are output tokens — the most expensive kind — spent producing telemetry for a scraper.
Structured mode carries tool calls, usage and turn boundaries natively. **No role prompt in zerg
should ever ask an agent to describe what it is doing for the orchestrator's benefit.**

### 11.2 The system prompt must be byte-frozen

Caching is a prefix match over `tools` → `system` → `messages`. One changed byte invalidates
everything after it.

§4.4 composes the system prompt fresh at every spawn from the database. That is correct for
staleness and **dangerous for caching**: interpolating anything volatile into it — task name, task
id, timestamp, worktree path, role position, run counter — changes the prefix on every spawn, so
nothing ever caches and the failure is silent (no error, just `cache_read_input_tokens: 0`).

Rule: the composed system prompt contains **shared instructions + role prompt and nothing else**.
Task-specific content belongs in the first user message, after the cached prefix. Everything the
role needs to know that varies per task travels in the work envelope, never in the system prompt.

Worth the discipline: cache reads cost ~0.1× input, writes 1.25× (5-minute TTL) or 2× (1-hour), so
break-even is two requests. A 3K-token composed prompt over a twenty-turn task costs roughly $0.18
uncached against $0.03 cached on Sonnet 5 — and the same ratio applies to the accumulated
conversation history, which is far larger. Minimum cacheable prefix is 1024 tokens on Sonnet 5 and
512 on Opus 5; a composed prompt below that silently will not cache at all.

### 11.3 Session lifecycle

Respawning a process per task means a cold session every time: the system prompt is re-sent and the
conversation restarts. Keeping one long-lived session per role lets the harness cache both the
system prefix and the accumulated history.

This is the second argument for the long-lived structured session of §7.2 — the first was
bidirectional input. Restart a cerebrate when its configuration changes or it crashes, not between
tasks.

Corollary for the role editor: changing a role's **model** invalidates every cache tier, since
caches are model-scoped. That is unavoidable and correct — the change requires a restart anyway —
but the UI should not present model switching as free.

### 11.4 Accounting rules

§11.1–11.3 are why cost moves. These are the rules for reporting it honestly.

**Record turns, not runs.** A run is a process; a turn is a billable unit. Only per-turn rows let you
attribute spend to a task, watch a cache rate change after a prompt edit, or find the role burning
the budget.

**Never report a bare "input tokens" number.** Prompt caching splits input three ways at wildly
different prices — uncached at 1×, cache writes at 1.25× or 2×, cache reads at ~0.1×. A dashboard
that sums them into one figure misstates cost by up to an order of magnitude and hides the single
biggest lever a user has. Store the three separately and show the split.

**Cache hit rate is a headline metric, not a detail.** It is the one number that reveals a silent
regression: a prompt edit that introduced a volatile byte drops the rate to zero and multiplies cost
with no error anywhere. A role whose rate falls below its own trailing average gets flagged.

**Prices carry effective dates.** A hardcoded table goes wrong on a schedule — Claude Sonnet 5 runs
at introductory $2/$10 per MTok through 2026-08-31 and $3/$15 after, so a table written this week is
wrong next week. Price rows are ranged and the lookup is by turn timestamp, so historical costs stay
correct after a price change rather than being retroactively rewritten.

**Distinguish metered from subscription.** This is the trap most likely to produce a wrong number
that looks right. An agent running under a Claude or ChatGPT subscription is not billed per token —
`pi` already reports this, printing `$0.067 (sub)` rather than a charge. Showing a subscription-run
role a confident "$47.32 spent" is simply false. Subscription turns are labelled and their dollar
figures presented as *estimated at API rates*, useful for comparing roles against each other and
useless as an invoice. Tokens are always real; dollars sometimes are not.

**Prefer the harness's own number.** When a harness reports cost, store it with
`cost_source = 'harness'`. Compute from the price table only when it does not, and mark it
`'computed'` so a disagreement is visible rather than averaged away.

## 12. History and metrics

The database makes history nearly free — but only if the three kinds of record are kept on different
terms. Treating them alike is how a local SQLite file becomes a 14 GB liability.

### 12.1 Three tiers, three retentions

| Tier | Row size | A busy day | Kept |
|---|---|---|---|
| `events` | ~2 KB (tool payloads, diffs, command output) | ~40 MB | rolling window, default 30 days |
| `usage_turns` | ~200 B | ~200 KB | indefinitely |
| `daily_rollup` | ~120 B | ~40 rows | forever |

The arithmetic decides it. Five roles at ~200 turns a day is ~1,000 turns; at 20 events per turn
that is roughly 40 MB of events daily — about **14 GB a year** — against ~73 MB of usage rows for the
same period. Events are the expensive tier and the least valuable in the long run: they exist to
replay and debug *recent* work.

The honest consequence, stated plainly in the UI: recent work replays in full; older work keeps its
metrics, its costs and its outcome, but not its complete transcript. The window is configurable, and
a task can be pinned to exempt it.

Rollups are computed on session end and on a daily timer, so a twelve-month chart reads a few
thousand tiny rows instead of scanning millions of turns.

### 12.2 Wall time is not work time

Two durations per task, and the gap between them is the interesting number:

- **Wall time** — `completed_at − created_at`. How long the task took in human terms.
- **Active time** — summed lease durations. How long agents actually worked on it.

A task showing 6 hours wall and 12 minutes active was not slow; it was **blocked** — waiting on an
approval gate, a clarification, or a queue behind another card. swarm-forge could not distinguish
these at all, so a stalled pipeline and a hard task looked identical. Charting them together turns
"where does our time go" from a guess into a reading.

### 12.3 What the history view answers

Every panel below is a query against `daily_rollup` joined to `tasks`, scoped by project:

- **Spend over time** — daily cost, stacked by role, with sessions marked on the axis. Answers
  "what did this project cost last month" directly.
- **Cost per task** — ranked. The long tail is usually fine; the top three are where the money went.
- **Wall vs active per task** — paired bars. Long wall with short active means the pipeline stalls,
  not the agents.
- **Cache rate over time** — a line, per role. This is the regression detector: the architect case in
  §11.4 shows up here as a cliff on the day its prompt changed, weeks before anyone would notice it
  in a bill.
- **Sessions** — a table: when, how long, roles active, tasks completed, cost.

### 12.4 Cross-project comparison

Because roles are global (§4.1) and rollups carry `project_id`, the same queries aggregate across
projects: cost per project, which project a role earns its keep in, whether a prompt change helped
everywhere or only in one repo. That falls out of the schema rather than needing a second one.

## 13. Artifacts — planned

*Not in the first build. Recorded now because it constrains the transport and storage decisions
above, and those are cheaper to get right than to retrofit.*

An artifact is anything an agent produces that a human wants to look at: a generated file, a
screenshot, a chart, a report, a diff — or a **running service**, a dev server the agent started that
you want to click and see.

### 13.1 Why not the WebSocket

The event stream is the wrong pipe for bytes. A 4 MB screenshot over WebSocket means base64
(+33% and a CPU cost at both ends), hand-rolled chunking, no browser caching, no range requests, and
it head-of-line blocks the live events behind it. Every one of those is solved by an HTTP GET.

So artifacts are ordinary HTTP resources. The event stream carries only the *announcement* — a small
`artifact` event with an id, kind and label — and the cockpit fetches bytes when the user asks for
them.

### 13.2 Producing one

```
zerg artifact add ./report.html --label "Coverage report"
zerg artifact add ./shot.png    --label "Login screen"
zerg artifact serve --port 5173 --label "Dev server"
```

Files are stored content-addressed under `~/.zerg/artifacts/<sha256>`, which dedupes the same output
across tasks and survives worktree cleanup. `serve` registers a port rather than bytes.

```sql
artifacts (id, project_id, task_id, role, kind,      -- file | image | service | diff
           label, sha256, mime, bytes, port, created_at, pinned)
```

### 13.3 Serving

- `GET /artifacts/{id}` — bytes, correct `Content-Type`, `ETag` = sha256, immutable caching, range
  requests. The browser does what browsers already do well.
- `GET /artifacts/{id}/preview` — inline render for the kinds that have one: images, text, diffs.
- `GET /proxy/{id}/*` — reverse-proxies a `service` artifact, so an app bound to `127.0.0.1:5173`
  is reachable from the cockpit without CORS, without exposing the port, and without the user
  hunting for which port an agent happened to pick.

### 13.4 The security constraint that shapes 13.3

A `service` artifact is **agent-generated code running in a browser**. Reverse-proxying it onto the
cockpit's own origin would give it same-origin access to cockpit state, session storage and the
command API — an agent bug, or a prompt injection in a file it read, could drive the orchestrator.

So proxied services are served from a **separate origin** (a distinct loopback port), embedded in a
sandboxed iframe, and never share the cockpit's origin or credentials. This is the reason `/proxy`
is specified as a reverse proxy on its own origin rather than a path on the main one, and it is
easier to build that way from the start than to unpick later.

### 13.5 Retention

Artifacts are the largest tier by bytes and slot into the §12.1 policy: content-addressed storage
dedupes, artifacts of completed tasks age out with their events on the same rolling window, and
`pinned` exempts anything worth keeping. A pinned artifact keeps its bytes even after its task's
transcript is gone.

## 14. Stack

Versions verified against npm dist-tags and `proxy.golang.org` on 2026-08-24.

### Go

| Component | Version | Path | Note |
|---|---|---|---|
| toolchain | **1.26.7** | — | 1.27.0 is days old; pin conservative. 1.27 needs macOS 13+ |
| router | stdlib | `net/http` | Go 1.22+ method+wildcard patterns suffice |
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
| reka-ui | 2.10.1 | the primitive layer; `radix-vue` was renamed to this and is frozen at 1.9.17 |
| Tailwind | 4.3.3 | CSS-first: no `tailwind.config.js`; use `@tailwindcss/vite` + `@theme {}` |
| @xterm/xterm | 6.0.0 | renamed from `xterm` (deprecated at 5.3.0); v6 **removed the canvas addon** — use WebGL |
| TypeScript | **6.0.3 — pinned** | TS 7 is the Go-native rewrite, shipping without a stable programmatic compiler API; `vue-tsc` (Volar) is reported pinned to TS 6 until ~7.1. `vue-tsc`'s peer range says `>=5.0.0`, which is misleading. Verify before unpinning |
| Pinia | 4.0.3 | |
| vue-router | 5.2.0 | |
| JetBrains Mono | — | Google Fonts; the preset's face for headings *and* body |
| HugeIcons | — | the preset's icon library, in place of Lucide |

### 14.1 UI foundation

The cockpit starts from shadcn-vue preset **`awF4GHI`** — Style *Lyra*, JetBrains Mono for headings
and body, **radius 0**, HugeIcons, Menu Default/Solid with a Subtle accent. Its structure is kept
verbatim; only the hue moves, from the preset's Mauve to the zerg palette. Tokens live in
[`web/theme.css`](web/theme.css) in shadcn-vue's own oklch format, so `npx shadcn-vue@latest add`
components inherit them with no per-component styling.

Two notes on that file:

- **Chart and tier colors are not decorative and must not be re-picked by eye.** `--chart-1..4`
  (role identity) and `--tier-1..4` (the cost ramp of §11.4) were validated against the dark surface
  and pass every gate — worst adjacent CVD ΔE 9.4. All four chart hues sit at L≈62%, which is *why*
  they pass. Changing one means re-running the validator, not nudging a hex.
- **Mono for body text is the preset's call, and it has a cost.** It suits dense tool UI — tables,
  logs, the activity view — and it is why the terminal skin and the cockpit chrome feel continuous.
  It is less comfortable for long prose, so watch the one screen that has any: the prompt editor. If
  it reads tiring at length, that editor is the place to make an exception, not the whole app.

> **Node floor:** `create-vue@3.23.0` requires Node `^22.18.0 || >=24.12.0`. This machine's shell
> resolves to v22.12.0 while nvm's default alias is v24.19.0 — scaffolding must run under 24.19.0.
> `.nvmrc` pins it rather than trusting ambient shell state; the same version-skew class broke `pi`
> locally.

---

## 15. Build order

1. **store + role library + project team API** — templates as rows with the eight built-ins seeded; a new project selects coder and reviewer.
2. **nydus + board** against an in-memory harness stub — prove leases, claims, acks, terminal merge
   with zero LLM calls.
3. **adapter interface + claude adapter + preflight**, including the readiness gate — a team that
   cannot work must not reach a running board.
4. **cerebrate** supervision, lease expiry, crash/backoff.
5. **Cockpit v1** — projects, team, role editor, attention, board.
6. **pi adapter** — the second adapter is what proves the interface is real.
7. pty attach + xterm.js; cost accounting; event replay.

Milestones 1–2 are deliberately LLM-free. The coordination layer is where the predecessor's failure
modes lived, and it is testable without spending a token.
