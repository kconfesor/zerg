# zerg architecture

A multi-agent coding orchestrator. Go core, Vue 3 cockpit, pluggable agent harnesses.

**Everything is configured in the UI.** There are no config files or prompt files to copy. You
define reusable teams, point zerg at a repo, and override only what that project needs.

Every "why" below traces to a failure that was watched happening, not one that was imagined. Some
were found running multi-agent orchestrators against real repositories; the rest are zerg's own,
from its first live task (§6.1).

---

## 1. Thesis

1. **The harness is an interface, not a branch in a switch statement.** Hardcoding a set of
   supported backends means adding one is an edit to the launcher, in every place the set appears.
   Adapters here are a Go interface with a registry.

2. **Agents emit events; they do not paint screens.** An orchestrator that infers status by
   grepping a terminal pane for a line containing `I'm`, and delivers work by injecting keystrokes
   into a TUI, is reimplementing a protocol on top of a display. Modern harnesses expose structured
   output (`pi --mode json|rpc`, `claude --output-format stream-json`). Consume that and the entire
   scraping layer disappears.

3. **Configuration is a database, not a filesystem.** Config spread across a conf file, a
   constitution, article fragments and per-role prompt files, then *snapshotted* into every
   worktree, means editing the original after launch changes nothing, silently.

   Verified the hard way: an edit setting the project language to Rust never reached a single
   agent, because the worktrees had been cut from a commit made before it. Six agents built the task
   in Clojure, with no warning. That incident is why this project exists. One database, one source
   of truth, composed fresh at every spawn.

4. **Failure must be loud at the boundary.** A corrupted global config, a CLI too old for its
   model, an unanswered trust dialog, a broken plugin tree. Four separate incidents in one day, all
   presenting identically as an agent that looked alive and did nothing. All four were detectable
   *before* spawning.

5. **A green board must mean the work happened.** Added after §6.1, which is the record of a task
   reaching Done over a branch that had never moved. Anything reporting success has to observe the
   thing it claims, not a proxy for it.

---

## 2. Scope boundary: provider setup

zerg **does not** manage provider credentials. It never runs a login flow, stores an API key, or
edits a harness's auth file. Users log into `pi`, `claude`, etc. themselves, using those tools.

zerg **does** detect credential state and say so plainly. `pi: no credentials for provider 'openai'
run /login in pi` is a blocked role with a remedy, not a silent hang. Detection is in scope;
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
| **role** | one running agent: a worktree, a harness process, a prompt, an inbox | `store.ResolvedRole`, `internal/cerebrate` |
| **team** | an ordered, enable-able list of roles: the pipeline work passes down | `store.TeamPreset` |

A role is not a label attached to a model. It is the unit that gets a process, a `.worktrees/<name>`
checkout and a queue, so "add a reviewer" means one more agent, one more worktree and one more
handoff, not a sentence in a prompt.

One binary, `zerg`. `zerg up` runs the daemon and opens the cockpit; `zerg next|done|send|ask` is
the agent-facing client (§7).

---

## 4. Configuration model

This is the part everything else depends on: get it wrong and no amount of care downstream helps.

### 4.1 Library, reusable team, project, runtime

Four layers make "configure once" and "every project is different" true at the same time.

**Role library**, global, in `~/.zerg/zerg.db`. A catalog of role *templates*: what a planner is,
what a reviewer is, what prompt each carries. Ships with a set of built-ins (§4.5); you edit them and
add your own. Editing a template changes the lowest default everywhere.

**Team**, named, and either shared by every project or belonging to one. It chooses library roles,
orders and enables them, and may specialize every role field. The built-in Default team is
coder → reviewer and is always shared, since it is where a new project starts.

Teams were global to begin with, and that was wrong in a way ownership fixes rather than explains: a
team carries the prompts, models and arguments one repository wants, so a global list put those in
front of every other repository, where adopting one was a click and editing it changed the first
project. `team_presets.project_id` is the separation. NULL is shared; set means that project's, and
then it is absent from every other project's picker, refused by `SetProjectTeam` if its id is posted
anyway, and deleted with the project. Moving a team to one project is refused while another runs it,
and names them; sharing one back is always allowed, since it strands nobody.

Team names stay unique across the installation rather than per owner. Making them per owner means
rebuilding `team_presets` to drop a UNIQUE constraint, and that table is what `team_preset_roles`
cascades from and what `projects.team_preset_id` points at, so the rebuild is an implicit delete of
every team's roles and every project's assignment. Not worth it for the ability to have two teams
called "Review" in one database.

**Project team**, per project. Every project runs exactly one team and follows later edits to its
pipeline and settings. Any role field can be overridden for that repository alone, which is the whole
of what a project layer is now: a prompt or a model for this repository, never a shape.

A project that wants a different pipeline gets a different team, one belonging to it. There used to
be a third possibility, a per-project topology that froze membership and order while still naming
somebody else's team, and it was removed in schema 16 because it made a project able to be "on" a
team and running something else, with two screens describing different layers and neither saying so.
The migration turned every such pipeline into a team owned by that project, carrying the settings the
named team had given its roles, and left projects whose frozen shape matched their team on the team
they already had.

**Runtime**, per project: tasks, messages, leases, events, cost. On disk a project holds only git
artifacts, `<repo>/.worktrees/<role>`.

Each layer is edited in one place, which is the point of splitting them: the library in
**Settings → Roles**, the reusable team and its per-team values in **Team**, and the project's own
values in the same view once that project is on the team. An edit's blast radius is therefore
readable off the control you used: global, one team, or one repository.

The project's *pipeline* is also editable from the rail beside the board (§10), because dropping a
role for one repository is decided while looking at that repository's work. What it edits is the
team, not a per-project copy of its shape: a team this project owns is written in place, and a shared
one is copied into this project first, named by whoever is making the change, with its per-role
settings carried across so the copy starts as the team that was running.

That is the whole rule, and it is what ownership bought. The alternative, tried first, was a
per-project topology layer, which is what §4.1 records the removal of: a project could be "on" a team
and running something else, and the rail and the Team screen would each report a different layer.

Every nullable override has one rule: null means inherit, while a value means local. For arguments,
`[]` is a value, meaning explicitly run with no role arguments, and remains distinct from null.

### 4.2 A role template, in full

Everything below is a form field in the UI. There is no other way to set any of it, by design.

| Field | UI control | Notes |
|---|---|---|
| name | text | also names the worktree: `.worktrees/<name>` |
| harness | select | populated from the adapter registry: `claude`, `pi` |
| model | combobox | options fetched live from the harness (§4.3); free text accepted |
| thinking | select | how hard the harness reasons, in its own levels; empty leaves its default |
| args | tag input | extra CLI flags, e.g. `--no-extensions` |
| receive | select | `task` (one at a time) or `batch` |
| batch policy | number + duration | `max_items`, `max_age`, only when receive is `batch` |
| prompt | editor | this role's instructions |
| gate | select | `none` or `approval`, to hold this role's handoffs for a human |

**Thinking** is passed through rather than translated. The two shipped harnesses spell it
differently and do not offer the same levels: claude takes `--effort` with low, medium, high, xhigh,
max, while pi takes `--thinking` and starts two levels lower at off and minimal. The adapter reports
its own list (`GET /api/harnesses/{name}/thinking`), the picker offers exactly that, and a harness
reporting none has no field at all. It costs tokens and time, so it is worth raising for the roles
that review and lowering for the ones that do not.

A team role adds **position**, **enabled**, and nullable defaults for every field above. A project
can override harness, model, args, receive/batch policy, prompt and gate, and only those: membership,
order and enablement belong to the team, so changing the pipeline means editing this project's team
or moving it to another one. Overriding is
explicit and visible: a role showing an override is badged in the team list, so a project that
quietly drifted from its reusable team is legible rather than mysterious.

Plus one **shared instructions** document, global, applied to every role. That single editable
document is the whole of it. There is no constitution file, no article fragments, no layering to
reason about.

### 4.3 Model discovery

Typing model ids by hand is how you get `Model metadata for 'gpt-5.6-sol' not found` and
`The 'gpt-5.6-sol' model requires a newer version of Codex`, which is twenty minutes of an agent looking
alive while every turn 400s.

So `Adapter.ListModels()` asks the harness what it can actually run (`pi --list-models`, claude's
alias set) and the UI renders a picker. The field still accepts free text, because a harness catalog
can lag a working model: `gpt-5.6-sol` is absent from pi's catalog and runs fine. Free text gets a
warning, not a block.

### 4.4 Prompt composition

At **every spawn**, the overmind composes `shared instructions + role prompt` from the database into
a temp file and hands it to the adapter. Nothing is copied into a worktree; nothing persists between
runs.

Consequence: edit a prompt in the UI, restart the role, and the change is live. This is the direct
fix for the snapshot staleness that silently produced a Clojure calculator when the config said Rust.

"Every spawn" is literal, and for a while it was not. Configuration was resolved once when the swarm
started, so a role that crashed and respawned came back with the prompt, model and flags it had when
the swarm went up, silently, and only for the roles that happened to crash. The supervisor now
re-reads the role from the database immediately before each spawn, and a role no longer on the team
stops instead of respawning.

### 4.4.1 What zerg injects, and what it leaves alone

An orchestrator that edits the repository it is orchestrating is a bad neighbour. The boundary:

**Appended, never replaced.** Both adapters use their harness's *append* flag
(`--append-system-prompt-file`, `--append-system-prompt`). The harness's own system prompt, the
project's `CLAUDE.md` or `AGENTS.md`, and the operator's global instructions all still load and
still apply. zerg adds the protocol and the role; it does not take over the agent.

**Nothing is written into the repository.** The composed prompt goes to
`$TMPDIR/zerg/<project>/<role>/state/<role>.system.md`, outside the tree. The only thing zerg puts
inside a repository is `.worktrees/`, and its ignore rule goes in `.git/info/exclude` rather than
`.gitignore`. That file belongs to the project, and writing to it made zerg's first task on a real
repository fail on a collision with the project's own.

**A ground rule forbids editing instruction files.** `CLAUDE.md`, `AGENTS.md` and their kin
configure every future agent, so a docs role tidying them would quietly change behaviour far beyond
its task. The shared instructions say to leave them alone unless the task is explicitly about them,
and to follow the conventions already in the repository over any preference of the agent's own.

**Role prompts name no paths the project has not chosen.** The planner writes a specification, and
looks for where the project already keeps design documents before falling back to `docs/specs/`.
An earlier version hardcoded that path, which meant pointing zerg at any existing repository
created a directory in someone else's tree on the first run.

**Budget.** Shared instructions are ~760 tokens; role prompts are 85–225. Under 1k per agent, and
byte-frozen per §11.2 so it is a cache hit after the first turn. Nothing instructs an agent to
narrate its status. Structured events carry that natively (§11.1), and a dashboard that greps for
sentences makes agents spend output tokens on telemetry.

### 4.5 The built-in library

Nine templates ship, covering every team shape worth presetting, as rows in a picker rather than
as branches of the orchestrator you have to check out to change your team.

| Template | Model | Receive | Gate | Does |
|---|---|---|---|---|
| `planner` | opus | task | **approval** | turns intent into a written spec, then waits for a human |
| `coder` | sonnet | task | none | implements the spec, writes tests, commits |
| `reviewer` | opus | batch | none | reviews the change against the spec, runs tests, reports or hands back |
| `debugger` | opus | task | none | reproduces a failure, finds the cause, fixes it behind a failing test |
| `cleaner` | sonnet | batch | none | behavior-preserving cleanup, duplication, dead code |
| `architect` | opus | batch | none | module boundaries, dependency direction, structural drift |
| `hardener` | sonnet | batch | none | edge cases, error paths, mutation-style probing |
| `security` | opus | batch | none | input handling, secrets, dependency and injection review |
| `docs` | sonnet | batch | none | README, API docs, changelog |

Reviewing roles run the stronger model on purpose: catching a wrong change is harder than making a
plausible one.

**A new project starts with `coder` then `reviewer` selected**, enough to be useful in two clicks.
Everything else is one checkbox away, and none of it is special-cased: a built-in is an ordinary row
you can edit, duplicate, or delete.

### 4.6 The planner and the approval gate

`planner` is the answer to "write the spec, then let me approve it before anything happens", and it
needs no new machinery: it is a template with `gate: approval`, a field roles already have.

The flow: planner writes the spec and commits it, then queues its handoff downstream. The gate holds
that handoff. **Attention** shows the task with a link to the spec itself, a `file` artifact
(§13) rendered inline rather than a filename you go hunting for, with **Approve** and **Reject**.
Approving delivers the handoff and moves the card; rejecting returns it to the planner with your note
attached, and nothing downstream ever saw it.

The gate is a field, not a role, so it composes: put `approval` on `architect` when a project needs
structural changes signed off, or on nothing at all for a repo you are happy to let run unattended.

---

## 5. Foundations

Four ideas the rest of the design is built on. They are load-bearing, and none of them is novel.
they are here because they were tried and they held.

- **Git worktree isolation per role.** One repo, one object store, N linked worktrees; peer commits
  resolve without a fetch. The single best structural idea available.
- **Commit-pointer handoffs.** A handoff points at a SHA; the receiver merges. Git already solved
  the hard parts of moving work between trees.
- **Human gates**: approvals and clarification requests surfaced in the UI.
- **A board of cards moving through lanes.** The right mental model for an operator.

One consequence worth stating outright: **every role gets a worktree**, and the repo root is the
integration branch, not a workspace. Letting one role occupy the repo root makes that role special
in the config, in routing, and on the board: a special case that has to be handled everywhere it
appears. When the terminal role completes, the *overmind* merges to the base branch. Integration
belongs to the orchestrator, never to whichever agent happened to be last.

## 6. Rejected approaches, and the failures behind them

Every row is a design that was tried and observed failing, not a hypothetical. They are recorded
because the reasoning is easy to lose once the code looks obvious.

| Rejected approach | Failure | What zerg does |
|---|---|---|
| Config as files, snapshotted into each worktree | Post-launch edits silently invisible to every agent; a Rust config produced a Clojure implementation | Database is the only source of truth; prompts composed fresh at spawn |
| Topology fixed by a conf file plus preset branches (`two-pack`, `four-pack`, `six-pack`) | Changing the team means checking out a different git branch of the orchestrator | Roles are rows, edited in the UI; the team *is* the config |
| Handoff state as files across N worktrees, root path inferred by git heuristics | Two possible outbox locations with independent sequence counters; filename-keyed dedupe silently **drops** a colliding message while still firing its wake-up | Single SQLite (WAL) database, one writer, real transactions |
| `deliver!` = N file copies + N notifies, then one move | Not transactional. Crash mid-loop re-delivers duplicates and re-moves the board. Nothing keys on message `id` | Outbox pattern in one transaction; idempotent on `(message_id, recipient)` |
| Wake-up = `tmux send-keys` of a fixed literal into the session's active pane | Lands in whatever is focused; hardcoded 150ms/50ms sleeps race the TUI's paste debounce; tmux exit 0 means "keys accepted", never "agent read it" | Agent **pulls** via `zerg next` (long-poll) over a unix socket |
| Lost wake-up recovery: none | Agent finishes → peeks empty inbox → prints `NO_TASK` → stops → mail arrives 5ms later → permanent stall, no timer, no retry | Leases with deadlines; unacked work returns to the queue and the role shows degraded |
| `PAYLOAD:` runs to EOF, unescaped | Any role can spoof protocol tokens at any other role in 80 chars, since `message: NO_TASK` yields a payload line reading exactly `NO_TASK` | JSON envelopes end to end |
| Check-then-move selection, no lock | Two concurrent claims create two batch dirs and split the queue; every later call errors `AMBIGUOUS_TASK_STATE` with **no recovery path** | Atomic claim: `UPDATE ... WHERE state='queued'` returning claimed rows |
| Helpers resolve the inbox from process cwd | Run from a subdirectory → creates an empty queue there and reports `NO_TASK`. False negative *with* a side effect | Identity from a spawn-time token in env; no path inference anywhere |
| Sender identity read from an environment variable, unvalidated | Any agent can export that variable as `architect` and send as the architect | Per-agent capability token minted at spawn |
| Terminal role = last line of the config file | Reordering the file silently relocates the end of the pipeline | A flag on the team's role, kept last by the daemon, shown as a `terminal` badge |
| Board lane moves at *enqueue* time | A card shows "in cleaner's lane" before cleaner has looked at it | Lane changes on **ack** |
| Batch = every equal-priority item at an instant | Unbounded and unfair; a priority-00 item arriving 1ms late waits behind a 40-item batch | Batch policy (`max_items`, `max_age`) set per role in the UI; priority preemption at claim time |
| Notes occupy the work queue | One 80-char informational note blocks a role's queue until an LLM turn consumes it | Two planes: work and control (§7.3) |
| Daemon reads socket/roles outside its try block | A deleted socket file terminates the transport *cleanly*, logging "stopped" and removing its pid, indistinguishable from normal shutdown | Supervised components with health endpoints |
| Nothing checks the harness before launching | 40 minutes lost to a triplicated `~/.codex/config.toml`, a CLI too old for its model, an unanswered trust dialog, a broken extension tree | **Preflight** (§8) |

### 6.1 What the first real run broke

The table above was written before an agent had ever run: every entry came
from watching an earlier design fail. This section comes from watching *this*
one fail, on its first live task, and it is kept because the entries share a
shape the original list does not have.

Those earlier failures were mostly **loud**: a stack trace, a stall, an
unrecoverable state with no way forward. Zerg's first failures were all
**quiet**: the board went green over work that had not happened. Quiet is
worse. A stall gets investigated within the hour; a false green ships.

| Mechanism | Failure | Fix |
|---|---|---|
| `Merged: m.CommitSHA != nil` in the work envelope | Nothing ever merged a hand-off into the recipient's worktree. The flag was inferred from the commit's presence, and the shared instructions said "do not merge it again" on that authority. A reviewer opened an empty tree twice | `Claim` merges into the role's worktree and reports the attempt's result |
| `--commit HEAD` stored as the literal string | `HEAD` names the tip of whichever tree resolves it. The coder's "my commit" became "main's tip" at the project root, where `merge --ff-only HEAD` is a no-op that returns success. A task reached Done with the base branch untouched | Refs resolved to absolute shas in the *sender's* worktree, at `Send` |
| `if req.Commit != ""` guarding the completion merge | An absent commit meant "integrate nothing, mark it done": the same green board over an unmoved branch, by a second route | Completion requires the commit to integrate |
| `--task` bound straight to a `REFERENCES tasks(id)` column | Agents are given a task *name* and told to keep it. Passing it back produced `FOREIGN KEY constraint failed` from inside the router, and the agent read that as "the recipient role is invalid" and asked an operator | `--task` accepts either form; a miss reports itself |
| Work envelope carried the payload but not the pipeline | An agent had no way to know who receives its output, so it guessed | Envelope carries `next` and `terminal`; the orchestrator resolves the team |
| Agents inherit the operator's `~/.claude` | claude reads OAuth from the keychain and will not start with a relocated config dir, so agents get the operator's MCP servers, plugins and hooks. A code-review agent held a live handle to a staging database, and an output-style plugin had it writing essays in output tokens | `--strict-mcp-config` removes the servers. Plugins and hooks still leak; `--bare` would stop them but also disables keychain reads |

**The pattern.** Five of the six are the same mistake: *a value that names an
outcome was derived from a proxy for that outcome rather than from the outcome
itself.* `merged` from the presence of a commit. Integration success from a
merge that was skipped. A ref's meaning from the tree that happened to read it.

Each one is individually obvious in hindsight and none was visible from the
outside, because the proxy and the fact agree in every case you would think to
check by hand. They diverge only under a real agent, in a real worktree, doing
real work, which is why they all surfaced within one task and none had
surfaced before.

**The test lesson is the sharper half.** The hand-off merge had a unit test. It
asserted:

    a handoff carrying a commit must say it was already merged

It read the same derived field the code wrote, so it restated the
implementation and could never contradict it. It stayed green through a
release in which no merge existed anywhere in the codebase.

Its replacement builds a real repository, commits in the sender's worktree, and
asks *git* whether the commit is an ancestor of the recipient's HEAD and
whether the file is on disk. The rule this establishes, and the reason these
notes exist: **a test for an effect must observe the effect in the system that
was supposed to change, not read back the field the code set.** Each
replacement test here was run against the reintroduced bug and confirmed to
fail before being kept.

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
          │ agent subprocess         │      │ browser: Vue 3 cockpit  │
          │  runs `zerg next|done|   │      │  configure · observe    │
          │  send` (same binary)     │      │                         │
          └──────────────────────────┘      └─────────────────────────┘
```

### 7.1 cerebrate

One per enabled role. Owns the agent process lifecycle and nothing else: preflight → spawn → parse
structured output into typed events → publish → track liveness → restart with backoff. It does not
decide routing; nydus does.

### 7.2 Agent-facing protocol

The agent's whole world is four verbs against a unix socket. Same binary, different subcommand, so no
PATH-synced script directory, no `.sh`/`.bb` wrapper pairs, no cwd inference.

```
zerg next [--wait 30s]   claim work (long-poll); JSON on stdout
zerg done [--result f]   ack the lease
zerg send --to <role> --commit HEAD --task <name>
zerg ask  "<question>" [--option "<one answer>" ...]
                         raise a clarification to the operator
```

Identity arrives as `ZERG_SOCKET` and `ZERG_TOKEN`, injected at spawn. The token is role-scoped and
per-spawn; there is no `--from` flag to forge.

**A question that is a choice says so.** Repeated `--option` turns the question into one, and
Attention draws it as radio buttons with **Something else** under them rather than a box to type in.
Options are stored as a JSON array on the clarification (`schema_034.sql`) and the answer stays one
string: the operator picks an option and that option's text is what the agent reads back, so it can
compare the answer to what it offered. Without this the enumeration the agent had already done
reached the operator as prose to read, decide and retype, and retyping is where the answer stops
matching the offer: an agent looking for one of three names gets a paraphrase, or a typo. No options
is the free-text question this started as, which is what every row written before 034 and every
agent that does not pass the flag still is. Blank or duplicate options are the agent's mistake and
come back as a 400 naming it, since two radio buttons a person cannot tell apart are worse than
prose.

**Asking again is asking the same question.** `ask` waits for a bounded time and then reports the
question as still open, so the agent decides what to do rather than hangs; what it does is ask
again. Filed twice, that put two identical cards in the panel with nothing to tell them apart, and
wrote the operator's answer to whichever one they reached first, which need not be the one an agent
was waiting on. Watched on one real card: two different options chosen six seconds apart, and the
agent acted on the second having never seen the first. A question is therefore identified by its
sender, its card, its wording and its offer, and a repeat joins the open row rather than filing a
new one. `clarifications.delivered_at` (`schema_035.sql`) records when an answer was actually handed
to an agent, so an answer given a moment after its asker gave up is collected by the next ask
instead of lost, while one that has been read is finished: asking the same thing later is a new
question and gets a new answer.

Work envelope:

```json
{
  "lease_id": "01JQ…", "task": {"id": "01JQ…", "name": "Calculator"},
  "from": "coder", "type": "handoff", "commit": "a1b2c3d4e5",
  "merged": true, "payload": "…", "expires_at": "2026-08-25T00:31:00Z"
}
```

`merged` states whether the overmind got that commit into the role's worktree, and it is set from
the merge attempt's result. Never infer it from the presence of a commit: §6.1 is what that costs.

**`next` and `terminal` are the card's, not the project's.** A task can be written to skip roles
(`tasks.skip`, §9), and then the route is the team's enabled roles minus those: the role after the
skipped one is `next`, and the last one left is `terminal`, so skipping the reviewer means the coder
merges. `store.Route` and `store.Onward` are the only two functions that decide this, because the
opening lane, the envelope and the check in `nydus.Send` that refuses a completion from a
non-terminal role have to agree — three copies of the filter disagreeing is not a visible bug, it is
a card routed one way and told it went another.

A lease therefore carries one route, which is why `nydus.Claim` will not batch two cards that skip
differently: batched together, whichever card lost would be handed the other's `next`.

Skipping governs automatic forward routing only, and only backward. An explicit `--to` still reaches
a skipped role behind the sender, because rework has to work: a reviewer that finds a problem on a
card whose coder was skipped still has to be able to send the work back, and that role is then told
to rejoin the route after itself. Forward into a skipped role is refused — nothing named it, so it
is a guess or a stale recipient, and it hands the card to the role somebody chose to leave out.

**Leases.** A claim has a deadline. Ack closes it; expiry returns the work to the queue and marks the
role degraded. This is the answer to "lost wake-up ⇒ permanent stall, no timer, no retry".

### 7.3 Two planes

- **work**: tasks and handoffs. Leased, priority-ordered, one unit (or one batch) at a time.
- **control**: notes, operator answers, cancellations. Out-of-band, never occupies a lease, never
  blocks work.

### 7.4 No tmux

A tmux-based design, one session per role, `send-keys` for delivery and `capture-pane` for the UI,
is the obvious way to build this, and zerg uses none of it. Agents are ordinary child processes of
the daemon, supervised with `os/exec`.

Every job tmux was doing has a better owner once agents emit structured events:

| tmux was providing | Now |
|---|---|
| process supervision | `os/exec` + cerebrate. Real exit codes and signals, restart with backoff, instead of a session that stays "alive" around a process returning HTTP 400 on every turn |
| somewhere for the TUI to live | Nothing to host in structured mode. Takeover allocates a pty directly (`creack/pty`); tmux was never needed for that |
| the delivery channel (`send-keys`) | Structured input over a pipe (§10.2) |
| the observation channel (`capture-pane`) | The typed event stream (§10.1) |
| per-project isolation via a socket path | The daemon owns every child; there is no shared namespace to collide in |
| **surviving the operator's terminal closing** | `zerg up --detach`, plus the restart path below, which puts back what the terminal took with it |

That last row is the only real loss, and tmux's version of it was weaker than it looked: it keeps a
*process* alive, which does nothing when the orchestrator has lost its *state*. Observed: a daemon
terminating cleanly on a missing socket file, logging "stopped" and removing its pid, indistinguishable
from a normal shutdown, while agents sat alive and idle and mail piled up in outboxes with no error
surfaced anywhere.

zerg answers it in three pieces:

- **Restart is a first-class path, not a recovery hack.** On restart every open lease is reclaimed
  immediately rather than being left to lapse, so claimed-but-unacked work is back in the queue
  before the first agent asks for any. An approval interrupted mid-integration is settled against
  the repository. Merged means the decision is recorded and the card closed, not merged means it
  goes back to the operator as pending. A session row left open by a daemon that was killed rather
  than asked is closed, so a work period does not read as one that never ended. Nothing has to be
  reattached, and nothing is silently half-delivered.

  This file used to say that if the daemon dies its children die with it, and that is true only of
  a daemon that is asked to stop. Each agent runs in a process group of its own (`Setpgid`, which
  is what lets a bash tool call's descendants be killed as a unit), and the group is signalled from
  `cmd.Cancel`, which a `SIGKILL`ed daemon never reaches. Measured: after `kill -9` on the daemon,
  a coder was still running thirty seconds later and still writing files into its worktree, and it
  exited on its own only some minutes afterwards. `zerg down` on the same swarm left nothing behind.
  Two consequences, and both matter more now that a restart puts a swarm back: an orphan and a
  resumed agent can hold the same worktree for a few seconds, and `--resume` can be aimed at a
  session that is still live, which is the case claude answers by forking to a new id. The fork is
  why the stored id is latched from the stream rather than trusted from what was sent. Killing the
  previous run's agents at boot is not implemented; a crash is currently survived rather than
  cleaned up after.
- **The swarm comes back, because somebody asked for it and nothing has un-asked.**
  `projects.start_requested_at` is the operator's standing intent, set on Start and cleared on Stop.
  A daemon shutting down clears nothing, so the next one starts what was running. `sessions.ended_at`
  cannot answer this and was tried: shutdown fills it in, so a clean stop and a killed daemon are
  the same row afterwards. Settings has a switch (`resumeOnStart`), because an unattended restart
  spends money; the conservatism is in the intent, not in the default.
- **Agents resume the conversation they were holding.** Both harnesses keep it on disk and will
  continue it, and the flags differ in a way that is not a detail: claude needs `--resume <id>`,
  because `--session-id` is the flag that *creates* one and exits with "Session ID <uuid> is already
  in use" when given a session it has already written; pi's `--session-id` creates or continues and
  serves both. The id is latched from the harness's own stream rather than remembered from what was
  passed, because claude answers `--resume` on a session that is still live by forking to a new id,
  and the fork is the conversation the work goes into. This is not only about daemon restarts: a
  cerebrate respawns a crashed agent with backoff, and that respawn was cold too.

  A session is stored with a fingerprint of the harness, model, thinking level and composed system
  prompt, and is not resumed when that changes. §11.3 restarts a role on a configuration change
  precisely so the change applies; resuming across the edit would replay a conversation shaped by
  the instructions that were just replaced while the board reported the new ones were in force.
  An operator's Stop forgets the conversations for the same reason it withdraws the intent: a
  process ending is not a decision and a person pressing Stop is, and continuing a week-old
  conversation about a finished task is continuity of the wrong thing.

`zerg up --detach` re-execs into a session of its own with `setsid`, so closing the terminal leaves
the daemon and its agents alone, and writes its output to `zerg.log` beside the database. The daemon
records itself in `zerg.pid` there, which is what `zerg down` and `zerg status` read and what stops
a second daemon opening the same database. It is deliberately not a service manager: it does not
restart the daemon and has no opinion about boot, which is what launchd and systemd are for.

The prerequisite list shrinks accordingly: Go and a logged-in harness. No tmux, no babashka, no zsh.

### 7.5 Transports

Four channels, each carrying what it is good at. "One WebSocket for everything" is a common instinct
and a bad one, since it reinvents request/response correlation, status codes, caching and range requests,
all of which HTTP already has.

| Channel | Transport | Carries |
|---|---|---|
| agent ↔ overmind | **unix socket** | `zerg next/done/send/ask`; never leaves the machine |
| cockpit → overmind | **HTTP/REST** | commands: create task, edit role, approve, start, stop |
| overmind → cockpit | **WebSocket** | the live typed event stream, and pty bytes during takeover |
| artifact bytes | **plain HTTP** | files, images, downloads, see §13 |

Commands are request/response with a status code and a body, so they belong on REST: a rejected
approval is a `409`, not a hand-rolled correlation id and an error frame invented for the occasion.

The WebSocket is multiplexed by frame type, so events and a takeover pty share one connection and one
auth path. On connect the client sends the last event id it saw and the server replays forward from
`events`, the same mechanism that makes a browser reload cost a replay rather than a rescrape
(§10.1).

This was built as SSE first, and then moved, which is worth recording because both directions have a
real case. SSE carries a one-way stream with materially less machinery: `EventSource` reconnects on
its own and resends `Last-Event-ID`, and since event ids are monotonic ULIDs that header *is* the
replay cursor. On a socket both are hand-written, meaning backoff with jitter, an explicit cursor frame and a
ping cadence, and every one of them is a thing that can be got wrong.

What settled it is that takeover needs keystrokes flowing back at typing latency, which SSE plus a
POST per keypress serves badly, and carrying two streaming transports until then would mean two
implementations of replay that drift. One mechanism beats two, and the second one is easier to
delete before it exists. A smaller reason found on the way: HTTP/1.1 allows about six connections
per origin, and an SSE stream holds one open per tab for its lifetime. The daemon serves plain TCP,
so there is no HTTP/2 multiplexing to make that free. WebSockets are not subject to that limit.

---

## 8. Preflight

Four hangs in one day of running an earlier build presented identically, as an agent that looks alive
and does nothing, and every one was detectable before spawning. Preflight is that check, promoted
from something you do when puzzled to something that runs first.

Runs before every spawn. Each check yields `ok` or `blocked(reason, remedy)`. A blocked role renders
in **Attention** with both, never as an idle pane that happens to be doing nothing.

| Check | Catches | Source |
|---|---|---|
| `binary_present` | harness not on PATH | generic |
| `binary_version` | *codex 0.134.0 cannot call `gpt-5.6-sol`* | adapter |
| `config_parses` | *triplicated `[features]` key in `~/.codex/config.toml`* | adapter |
| `auth_valid` | *pi: "No API key found for openai"* → "log in with pi" (detect only, §2) | adapter |
| `workspace_trusted` | *claude's first-run trust dialog blocking four roles* | adapter |
| `model_available` | model id absent from the harness catalog → warn, don't block (`Advisory`) | adapter |
| `plugins_loadable` | *pi's broken extension tree*, smoke run with the real flags | adapter |

### 8.1 Two moments, one check suite

The same checks run at two points, because the two failures they prevent are different.

**Project setup, the readiness gate.** Adding a project, or pressing Start, runs the full suite
across **every enabled role** first, in parallel, and renders a readiness panel: one row per role,
each check green, amber or red, with the remedy inline for anything failing. Start is disabled while
any role is red.

This is the moment that matters. Half our lost day came from a swarm that launched *successfully*:
six sessions up, dashboard green, board drawn, while four roles sat at a trust dialog and two more
were dead on a config parse error. Nothing was wrong with the launch; everything was wrong with the
agents. A team that cannot work should never reach a running board.

Red is blocking. Amber (an unlisted model, a harness whose version could not be determined) shows a
warning and allows Start with an explicit acknowledgement, since a catalog can lag a model that
works.

Some checks can only ever be amber, and say so: `adapter.Check.Advisory` marks a probe whose worst
honest verdict is a warning, and preflight will not let one turn red however it fails. Without that,
the paths that produce a finding nobody wrote went the other way. `model_available` reports an
unlisted model as a warning and a missing catalog as nothing at all, and then blocked a team on a
machine busy running four agents, because `pi --list-models` answered in under two seconds idle and
took longer than the ten-second budget under that load. A slow answer to a question that cannot
block should not block. The panel is re-runnable on demand: you fix a login in another terminal, hit **Re-check**,
and watch the row go green without restarting anything.

**Spawn, the guard.** The same suite runs again before each individual spawn, because state drifts
between setup and launch and between one task and the next: a token expires, a `brew upgrade`
replaces a binary, another tool rewrites a shared config. A role that fails here does not spawn; it
appears in Attention as blocked with its remedy, and the work it would have claimed stays queued
rather than vanishing into a lease held by a process that cannot run.

Checks are cheap (a version probe, a config parse, a credential read) and cached briefly per role,
so the spawn guard costs milliseconds.

### 8.2 Isolated harness config

Observed: two codex agents launched 1.5s apart into fresh directories, both doing a non-atomic
read-modify-write of the **global** `~/.codex/config.toml` to register trust. The writes raced,
producing a file containing three concatenated copies of itself, which then failed to parse for every
codex invocation on the machine, including unrelated projects.

Each cerebrate therefore gets a private harness config directory (`CODEX_HOME`,
`PI_CODING_AGENT_DIR`, …) seeded from the user's real one. Agents never write shared global state.
Adapters declare which env var relocates their config.

---

## 9. Data model

SQLite, WAL, single writer inside the overmind. Migrations are numbered files, applied in one
transaction, and `user_version` records how far a database has got.

```sql
-- global library, edited exclusively through the UI
role_templates (id, name, harness, model, args, receive,
                batch_max_items, batch_max_age_sec, prompt, gate,
                builtin, created_at, updated_at)
settings       (key, value)       -- shared instructions, daemon config, harness flags

team_presets       (id, name, builtin, project_id, created_at, updated_at)
team_preset_roles  (preset_id, template_id, position, enabled,
                    harness_override, model_override, args_override,
                    receive_override, batch policy overrides,
                    prompt_override, gate_override)

projects       (id, path, name, base_branch, created_at, last_opened_at,
                integration, pr_draft, team_preset_id,
                icon,                       -- a file inside the repo, resolved and served (§10)
                chat_harness, chat_model)

-- sparse per-field layer over the team the project runs. Fields only: a project
-- wanting a different pipeline runs a team of its own (§4.1)
project_role_overrides (project_id, template_id,
                        harness_override, model_override, args_override,
                        receive_override, batch policy overrides,
                        prompt_override, gate_override)

-- per-project runtime
sessions    (id, project_id, started_at, ended_at, end_reason)

tasks       (id, project_id, session_id, name, body, lane, state,
             created_at, first_claimed_at, completed_at,
             active_ms,           -- summed lease time; wall time is completed−created
             rework_count,        -- laps backward through the pipeline
             deploy,              -- where this card's work is put when it lands; empty for most
             skip,                -- role template ids this card does not visit (§7.2); empty for most
             hidden)              -- put away by a person; §9.3

messages    (id, project_id, task_id, from_role, kind, priority,
             commit_sha, body, terminal, created_at)
routes      (message_id, to_role, state, enqueued_at, delivered_at)  -- idempotent per recipient
leases      (id, project_id, role, granted_at, expires_at, acked_at, expired_at)
lease_items (lease_id, message_id, to_role)   -- a batch lease covers many messages
events      (id, project_id, task_id, role, kind, ts, text, tool, data, fatal)  -- append-only
approvals   (id, project_id, message_id, state, note, created_at, decided_at)
clarifications (id, project_id, task_id, role, question, answer, state,
                created_at, answered_at)

-- one row per model turn, not per run: this is what the cost dashboard reads
usage_turns (id, project_id, task_id, role, ts,
             harness, provider, model,
             input_tokens,        -- uncached input only
             cache_write_tokens,  -- billed ~1.25x (5m TTL) or 2x (1h)
             cache_read_tokens,   -- billed ~0.1x
             output_tokens,
             cost_usd,
             cost_source,         -- 'harness' | 'computed'
             billing)             -- 'metered' | 'subscription'
```

**The recorder does not share the bus's semantics.** The event bus drops when a subscriber's buffer
fills, which is right for a browser and wrong for the writer of the usage rows the cost accounting is
made of: measured, a 5,000-event burst against the old inline recorder stored 1,025 of them and lost
the rest without a word. The bus channel is now drained into an unbounded queue by a reader that does
nothing else, and the database writes happen behind it, so a backlog costs memory, which is visible
and bounded by the run, rather than rows, which are not recoverable. `/api/health` reports the
queue depth, peak, drops and write failures, and answers `degraded` rather than `ok` when the record
has gaps.

`events` being append-only gives the cockpit free time travel: the UI is a projection, so a reload
replays rather than re-scrapes. Ids are monotonic ULIDs, which makes one value serve as primary key,
sort order and stream resume cursor at once: a client reconnects with `after=<last id seen>`.

### 9.1 The two gates

A role's `gate` column decides whether its output stops for a person. Both kinds of stop use the
same `approvals` table and the same cockpit queue, but they hold different things:

- **A routed handoff**: the sender is not terminal. The route is written `held` rather than
  `queued`, so the recipient never sees it. Approving flips it to `queued`.
- **A terminal completion**: the sender is the role flagged terminal, and approving is what lands the
  work. Integration runs *before* the decision is recorded, so a merge that fails leaves the
  approval open rather than marking a task done over a branch that never moved. §6.1 is the record
  of getting that backwards.

The distinction reaches the cockpit as `terminal` on the approval, because the two deserve
different views. A handoff shows what the sender just committed. A terminal completion shows
`git diff base...sha`, everything that would land across every role and every lap, since
approving the last commit on the strength of its own diff is approving a merge by its closing
paragraph.

### 9.2 Where finished work goes

`projects.integration` is per project and deliberately not per role:

| mode | what the terminal approval does |
|---|---|
| `merge` | fast-forwards the base branch. Right for a repository you own outright. |
| `pr` | pushes the branch and opens a PR via `gh`, using the handoff note as the description; `projects.pr_draft` adds `gh pr create --draft`. |
| `branch` | nothing. The work sits on its branch; landing it is a later decision. |

Per role would attach the policy to whichever role happened to be last, and disabling that role
would silently move it to another one, the same failure as taking terminality from config-file
line order, which §6 records.

### 9.3 Hidden cards

`tasks.hidden` is a card a person has put away. Done accumulates, and most of an old board is work
nobody needs to see again. An age cutoff was the alternative and hides the wrong things: a
month-old card you still refer to goes, this morning's dead end stays.

It is a column rather than a browser preference because the same board is read from a laptop and a
phone over the tailnet, and a card put away on one should be away on the other. Whether hidden
cards are *shown* is per browser, and lives in `localStorage`.

Hiding changes nothing else: not the lane, not the state. A hidden card is finished work that is
still finished, and unhiding returns it unchanged.

## 10. Cockpit

`web/`, built by Vite, embedded with `//go:embed all:dist`. The `all:` prefix is required, since
plain `//go:embed dist` silently skips Vite's `.vite/` manifest directory.

**Configure**

- **Projects**: list, add by absolute path (checked to exist and be a directory), set base branch,
  name, icon and what an approval does. Two clicks to a running swarm.
- **Readiness**: the preflight panel (§8.1). One row per enabled role, every check with its status
  and an inline remedy, and a **Re-check** button. Start is not disabled while a role is blocked, it
  **refuses** and returns the report. A disabled button says only that it cannot be pressed,
  whereas the refusal says which role is blocked and what to do about it.
- **Team**: one three-column master-detail view over *one layer*, the reusable team. **Teams**
  lists them in two groups, this project's own and the shared ones, with a control on each row that
  moves it between the two, and a clone that lands in this project unless it is asked to be shared.
  It lists Default and its clones, **Roles** adds library roles to the selected team and opens their
  per-team settings, and **Pipeline** orders and enables them. Selecting a team edits it; **Use this
  Team** separately assigns it to the current project, so browsing never silently changes what runs.
  A banner names the mismatch when the team on screen is not the one this project is on, and says
  instead that edits apply immediately when agents are running.
- **The rail's pipeline editor**: the same list that shows what each role is doing turns into an
  editor for the team behind it, to add a role, turn one off, reorder it or remove it. On a team this
  project owns, each change is written and reconciled within the second. On a shared team, the first
  change opens a dialog naming the copy it is about to make for this project, since the alternative
  is changing a pipeline every other project on that team runs. The rail says which of the two will
  happen before the first click, the dialog carries any refusal (a duplicate name is a 400), and the
  last enabled role will not turn off or be removed, since a team with nothing enabled cannot start
  and has nowhere to route a task.
- **Role editor**: every field in §4.2, for one role within whichever layer opened it: the team
  editor writes team-level values, a project's own team writes project-level ones. Each field states
  what it inherits and offers **Use default** to drop back to it, and the dialog counts how many
  fields are overridden, so the layer you are writing to is never ambiguous.
- **Settings**: the global layer, in tabs. **Roles** is the library editor (§4.1), the definition
  of what each role *is*, editable in exactly one place because an edit here reaches every team and
  every project, with each entry showing which teams use it and a delete that names them before it
  cascades; **Instructions** is the shared prompt document applied to every role; plus **Network**,
  **Disk** and **Harness**.

**Observe**

- **Attention**: blocked preflights (with remedies), approvals, clarifications. Anything needing a
  human, first. A dialog rather than a route, deliberately: what is waiting interrupts whatever you
  are reading, and sending you to another page to answer it loses your place both ways. Errors from
  a decision taken in it render inside it, since the page behind is not visible on a phone.
- **Board**: one lane per enabled role plus Done. Cards move on ack.
- **Activity**: the event stream for one card, replayed from `events`: every turn, tool call and
  handoff in order (§10.1).
- **Roles**: per-role health, current lease, live/idle, tokens, cost. In the sidebar, not a route.
- **Spend**: the cost dashboard (§11.4). Tiles carry tokens, dollars and cache rate for the
  selected window; a range control (session / day / week / month / all) and, once more than one
  provider has been used, provider chips that scope the whole page. Breakdowns by **role**, meaning a
  stacked bar of the three-way input split, coloured by unit price rather than by rank, with a table
  saying the same thing in numbers, and by **provider**, with subscription rows labelled as
  estimates. A callout names any role whose cache rate has fallen against its own trailing average,
  since that is the failure that costs money silently (§11.2). Per-task and per-turn drill-down is
  not built; the activity view is where a single card's turns are read.
- **History, planned** (§12.3). The long view, scoped per project: spend over time stacked by role, cost per
  task ranked, wall time against active time, cache rate as a line, and a session log. Reads
  `daily_rollup`, so a twelve-month range is as fast as a one-day range.
- **Chat**: talk to the first role in the pipeline.

Transport: one WebSocket carrying typed events; REST for commands.

### 10.1 Watching an agent work

An agent in structured mode is not painting a screen, so there is no TUI to attach to. A pty on
that process shows JSON lines scrolling past. Three modes cover what a terminal was being used for,
and the first is better than the thing it replaces.

**Activity view** *(default)*. Rendered from the structured stream: every tool call, every `bash`
command with its stdout and exit code, every file edit as a diff, reasoning, errors. This is the
"what is it doing right now" view. Because it is structured rather than scraped, it is searchable,
filterable by role or tool, linkable per event, and replayable from the `events` table after a
reload. A terminal scrape offers none of that; the version of this question it can answer is
"does the pane contain a line matching `I'm`".

**Raw stream**. The JSON lines as received. For debugging an adapter, not for watching work.

**Interactive takeover** *(on demand)*. Sometimes you genuinely want the harness's own TUI, to run
its slash commands, or to drive it by hand. That is a deliberate mode switch, not a second view of
the same process: the cerebrate stops the headless process and relaunches that one role in its
native TUI on a pty, which the cockpit renders with xterm.js. Structured events pause for the
duration and the role is marked `takeover` on the board, because the orchestrator can no longer see
what it is doing. Detaching relaunches it headless.

### 10.2 Talking to a running agent

Both target harnesses accept **streaming structured input** alongside streaming output
(`claude --input-format stream-json`, `pi --mode rpc`). Chat messages, clarification answers and
follow-ups are therefore delivered as structured messages to a running agent.

No keystrokes are ever injected. Keystroke delivery means sending a fixed literal to whichever pane
happens to be focused, with hardcoded sleeps racing the TUI's paste debounce, and an exit code of 0
that means "keys accepted" and never "the agent read it". Delivery here is a write to a pipe with a
response event to confirm it landed.

### 10.2.1 The brief editor

A task's brief is the whole of what an agent is told, so the box it is written in matters more than
its size suggests. **What is stored is Markdown**, and that is the fixed point: it goes to the
harness as text, Markdown is what these models read natively, and it is already the format agent
output comes back in. An editor that stored HTML would hand the agent tags to read past.

The editing surface is rich text over that Markdown: TipTap, which is a ProseMirror document with a
schema, parsed from Markdown on the way in and serialised back on every change.

The surface changed because the plain one was failing at things that are not features. It was a
textarea whose toolbar spliced literal characters into the value, so: writing to the model directly
meant the browser's undo stack never saw a toolbar edit and Cmd-Z could not undo a bold; pressing
**Bold** twice produced `******like this******` rather than turning the mark off; Enter did not
continue a list and Tab left the field. In a schema those are properties rather than features: a
mark toggles, history is a real transaction log, a list item is a node.

Two things follow, and both are deliberate:

- **The round trip is lossy for anything the schema does not model.** The **Source** tab is
  therefore part of the design, not a debug view: it shows exactly what will be sent, and can be
  edited as text when the editor is in the way.
- **`html: false` on the serialiser.** Raw HTML in a brief stays literal text instead of becoming
  nodes, which keeps this consistent with the renderer used for agent output, where escaping before
  anything else is a security property rather than a formatting one.

---

## 11. Token economics

zerg does not call the Messages API; the harnesses do. What zerg controls is the *bytes it hands
them* and *how often it restarts them*, and both decide whether prompt caching works.

### 11.1 Output format costs nothing

`--output-format stream-json` is a serialization choice for the CLI's stdout. It changes how the
harness prints what it already received; the request and the completion are identical. Structured
mode is not more expensive than a TUI.

It is slightly *cheaper*, for one reason. A dashboard that reads status by grepping a pane has to
instruct every agent to narrate its status in prose. Those are output tokens, the most expensive kind,
spent producing telemetry for a scraper.
Structured mode carries tool calls, usage and turn boundaries natively. **No role prompt in zerg
should ever ask an agent to describe what it is doing for the orchestrator's benefit.**

### 11.2 The system prompt must be byte-frozen

Caching is a prefix match over `tools` → `system` → `messages`. One changed byte invalidates
everything after it.

§4.4 composes the system prompt fresh at every spawn from the database. That is correct for
staleness and **dangerous for caching**: interpolating anything volatile into it, whether task name, task
id, timestamp, worktree path, role position or run counter, changes the prefix on every spawn, so
nothing ever caches and the failure is silent (no error, just `cache_read_input_tokens: 0`).

Rule: the composed system prompt contains **shared instructions + role prompt and nothing else**.
Task-specific content belongs in the first user message, after the cached prefix. Everything the
role needs to know that varies per task travels in the work envelope, never in the system prompt.

Worth the discipline: cache reads cost ~0.1× input, writes 1.25× (5-minute TTL) or 2× (1-hour), so
break-even is two requests. A 3K-token composed prompt over a twenty-turn task costs roughly $0.18
uncached against $0.03 cached on Sonnet 5, and the same ratio applies to the accumulated
conversation history, which is far larger. Minimum cacheable prefix is 1024 tokens on Sonnet 5 and
512 on Opus 5; a composed prompt below that silently will not cache at all.

### 11.3 Session lifecycle

Respawning a process per task means a cold session every time: the system prompt is re-sent and the
conversation restarts. Keeping one long-lived session per role lets the harness cache both the
system prefix and the accumulated history.

This is the second argument for the long-lived structured session of §7.2. The first was
bidirectional input. Restart a cerebrate when its configuration changes or it crashes, not between
tasks.

A crash or a daemon restart is not a reason to pay for a cold session either, which is what §7.4's
resume is for: the harness is handed back the conversation it was writing to, so the accumulated
history is a cache read rather than a re-send. Measured against claude 2.1.258, a resumed process
read 21511 tokens from cache on its first turn.

Corollary for the role editor: changing a role's **model** invalidates every cache tier, since
caches are model-scoped. That is unavoidable and correct, since the change requires a restart anyway,
but the UI should not present model switching as free. It is also why a stored session carries
a fingerprint of what it was started under, since a resume into a different model would be a cache
miss dressed as a continuation.

### 11.4 Accounting rules

§11.1–11.3 are why cost moves. These are the rules for reporting it honestly.

**Record turns, not runs.** A run is a process; a turn is a billable unit. Only per-turn rows let you
attribute spend to a task, watch a cache rate change after a prompt edit, or find the role burning
the budget.

**Never report a bare "input tokens" number.** Prompt caching splits input three ways at wildly
different prices: uncached at 1×, cache writes at 1.25× or 2×, cache reads at ~0.1×. A dashboard
that sums them into one figure misstates cost by up to an order of magnitude and hides the single
biggest lever a user has. Store the three separately and show the split.

**Cache hit rate is a headline metric, not a detail.** It is the one number that reveals a silent
regression: a prompt edit that introduced a volatile byte drops the rate to zero and multiplies cost
with no error anywhere. A role whose rate falls below its own trailing average gets flagged. Built,
and deliberately quiet: the trailing rate has to have been above 0.4, the fall at least 0.2, with at
least three turns either side of the edit and at most 200 turns read. Under those bars a fall is
noise, and a flag that fires on noise is one people learn to scroll past. The callout states the
multiple the role is now paying on input, computed on blended prices. The arithmetic that divides
uncached fractions instead prices a cache read at zero and overstates it several-fold.

**Prices carry effective dates.** A hardcoded table goes wrong on a schedule: Claude Sonnet 5 runs
at introductory $2/$10 per MTok through 2026-08-31 and $3/$15 after, so a table written this week is
wrong next week. Price rows are ranged and the lookup is by turn timestamp, so historical costs stay
correct after a price change rather than being retroactively rewritten.

**Distinguish metered from subscription.** This is the trap most likely to produce a wrong number
that looks right. An agent running under a Claude or ChatGPT subscription is not billed per token.
`pi` already reports this, printing `$0.067 (sub)` rather than a charge. Showing a subscription-run
role a confident "$47.32 spent" is simply false. Subscription turns are labelled and their dollar
figures presented as *estimated at API rates*, useful for comparing roles against each other and
useless as an invoice. Tokens are always real; dollars sometimes are not.

**Prefer the harness's own number.** When a harness reports cost, store it with
`cost_source = 'harness'`. Compute from the price table only when it does not, and mark it
`'computed'` so a disagreement is visible rather than averaged away.

## 12. History and metrics

> **Status: partly built.** `usage_turns` exists and is what the cost panel
> reads, §12.1's sweep runs on a timer against a configurable window, and §12.3
> is built: the History screen, a task's trail, the slice of transcript behind
> each step, and a pin that exempts a card from the sweep. `daily_rollup`, the
> price table and the charts in §12.4 are described below as the design they will
> follow and are **not implemented**. §9 lists the tables that actually exist.

The database makes history nearly free, but only if the three kinds of record are kept on different
terms. Treating them alike is how a local SQLite file becomes a 14 GB liability.

### 12.1 Three tiers, three retentions

| Tier | Row size | A busy day | Kept |
|---|---|---|---|
| `events` | ~2 KB (tool payloads, diffs, command output) | ~40 MB | rolling window, default 30 days |
| `usage_turns` | ~200 B | ~200 KB | indefinitely |
| `daily_rollup` | ~120 B | ~40 rows | forever |

The arithmetic decides it. Five roles at ~200 turns a day is ~1,000 turns; at 20 events per turn
that is roughly 40 MB of events daily, about **14 GB a year**, against ~73 MB of usage rows for the
same period. Events are the expensive tier and the least valuable in the long run: they exist to
replay and debug *recent* work.

The honest consequence, stated plainly in the UI: recent work replays in full; older work keeps its
metrics, its costs and its outcome, but not its complete transcript. The window is configurable, and
a task can be pinned to exempt it.

Both halves of that are built. History says which cards still have a transcript, asked of the events
table rather than worked out from the window, because a sweep that has not run yet and a window that
was lengthened afterwards both make that arithmetic wrong. Pinning holds on the delete itself rather
than in a caller. Demonstrated against a copy of a real database with the window set to a day: 812
events swept, the pinned card keeping all 166 of its own, and every card keeping its cost, its
outcome and its four-step trail.

Rollups are computed on session end and on a daily timer, so a twelve-month chart reads a few
thousand tiny rows instead of scanning millions of turns.

### 12.2 Wall time is not work time

Two durations per task, and the gap between them is the interesting number:

- **Wall time**: `completed_at − created_at`. How long the task took in human terms.
- **Active time**: summed lease durations. How long agents actually worked on it.

A task showing 6 hours wall and 12 minutes active was not slow; it was **blocked**, waiting on an
approval gate, a clarification, or a queue behind another card. Without that distinction a stalled
pipeline and a genuinely hard task look identical. Charting them together turns
"where does our time go" from a guess into a reading.

### 12.3 What a task's own history answers

> **Built.** The rest of this section, the charts over `daily_rollup`, is still
> the design it will follow.

A **History** screen per project lists what was worked on, newest first, paged on
a cursor rather than an offset because the list is read while work continues.
Each row carries its outcome (§9.2, recorded on the card rather than
reconstructed from a project setting that can change), wall time against active
time, cost, laps, and the roles that touched it.

Opening one gives the **trail**: every hop, with how long that role held the
work, what its turns cost, the approval it waited on and for how long, and the
questions it raised. Two things the schema decides here:

- A step's window runs from one of a role's leases to that role's *next* one,
  not to the handoff between them. The turn that ends a step is recorded after
  the handoff it produced, since the agent calls `zerg send` and the turn
  carrying that call finishes afterwards. Closing at the handoff put the largest
  turn of every step outside it: one real card totalled $1.61 with $0.23 across
  its steps.
- Messages carried no `source_lease_id` before schema 11, so those steps have no
  lease to measure from and fall back to the span between the handoff that gave
  the role the work and whatever it did next. Weaker, and better than a card
  whose steps all read $0 against a card reading $2.74.

A step opens its own slice of the transcript, bounded by that same window and
filtered to what a person reads: the calls it made, what it said, what broke.
The rest is counted rather than listed. Events are the tier that ages out
(§12.1), so an empty slice is an ordinary answer and says so.

### 12.4 What the history charts will answer, planned

Every panel below is a query against `daily_rollup` joined to `tasks`, scoped by project:

- **Spend over time**: daily cost, stacked by role, with sessions marked on the axis. Answers
  "what did this project cost last month" directly.
- **Cost per task**, ranked. The long tail is usually fine; the top three are where the money went.
- **Wall vs active per task**: paired bars. Long wall with short active means the pipeline stalls,
  not the agents.
- **Cache rate over time**: a line, per role. This is the regression detector: the architect case in
  §11.4 shows up here as a cliff on the day its prompt changed, weeks before anyone would notice it
  in a bill.
- **Sessions**: a table of when, how long, roles active, tasks completed, cost.

### 12.5 Cross-project comparison

Because roles are global (§4.1) and rollups carry `project_id`, the same queries aggregate across
projects: cost per project, which project a role earns its keep in, whether a prompt change helped
everywhere or only in one repo. That falls out of the schema rather than needing a second one.

## 13. Artifacts

*Built. `zerg artifact add` and `zerg artifact serve`, schema 025, and a second listener for
proxied services. What follows describes what is there; the deployment half of issue #9 is not.*

An artifact is anything an agent produces that a human wants to look at: a generated file, a
screenshot, a chart, a report, a diff, or a **running service**: a dev server the agent started that
you want to click and see.

### 13.1 Why not the WebSocket

The event stream is the wrong pipe for bytes. A 4 MB screenshot over WebSocket means base64
(+33% and a CPU cost at both ends), hand-rolled chunking, no browser caching, no range requests, and
it head-of-line blocks the live events behind it. Every one of those is solved by an HTTP GET.

So artifacts are ordinary HTTP resources. The event stream carries only the *announcement*, a small
`artifact` event with an id, kind and label, and the cockpit fetches bytes when the user asks for
them.

### 13.2 Producing one

```
zerg artifact add ./report.html --label "Coverage report"
zerg artifact add ./shot.png    --label "Login screen"
zerg artifact serve --port 5173 --label "Dev server"
```

Files are stored content-addressed under `~/.zerg/artifacts/<sha256>`, which dedupes the same output
across tasks and survives worktree cleanup. `serve` registers a port rather than bytes, and dials it
first: a typo otherwise becomes a link that fails only when somebody clicks it.

Two rules the implementation added. A file has to come from the project's tree, symlinks resolved
before the check — an agent can already run code in its worktree, so this is not a wall, but without
it one line in a poisoned file (`zerg artifact add ~/.ssh/id_rsa`) copies a key onto an HTTP surface
with no authentication. And the card is inferred from the lease the role is holding, like a
clarification's, with `--task` to override.

```sql
artifacts (id, project_id, task_id, role, kind,      -- file | image | service | diff
           label, sha256, mime, bytes, port, created_at, pinned)
```

### 13.3 Serving

- `GET /api/artifacts/{id}/bytes`: bytes, correct `Content-Type`, `ETag` = sha256, immutable
  caching, range requests. The browser does what browsers already do well.
- `GET /api/tasks/{id}/artifacts`: what a card produced, with a `url` for each running service.
- The service origin's `/{id}/*`: reverse-proxies a `service` artifact, so an app bound to
  `127.0.0.1:5173` is reachable from the cockpit without CORS, without exposing the port, and
  without the user hunting for which port an agent happened to pick.

There is no `/preview`: the cockpit decides how to render from the type, and only pictures and plain
text are ever rendered inline. HTML and SVG are downloads, SVG because it is a document format that
can carry script.

### 13.4 The security constraint that shapes 13.3

A `service` artifact is **agent-generated code running in a browser**. Reverse-proxying it onto the
cockpit's own origin would give it same-origin access to cockpit state, session storage and the
command API. An agent bug, or a prompt injection in a file it read, could drive the orchestrator.

So proxied services are served from a **separate origin** (a distinct loopback port), embedded in a
sandboxed iframe, and never share the cockpit's origin or credentials. This is the reason the proxy
is a reverse proxy on its own origin rather than a path on the main one, and it is easier to build
that way from the start than to unpick later.

As built: the port is chosen by the operating system, not configured, because it is an
implementation detail of a link the daemon builds itself from the request that asked — loopback for
a browser here, the tailnet name for a phone. It binds loopback only and serves TLS with the
cockpit's certificate when there is one, since an https page cannot embed an http iframe. That
origin serves proxied services and nothing else: no cockpit, no API, and no redirect back to
either, because a link from the untrusted origin to the trusted one is the shape of the thing this
prevents.

### 13.5 Retention

Artifacts are the largest tier by bytes and slot into the §12.1 policy: content-addressed storage
dedupes, artifacts of completed tasks age out with their events on the same rolling window, and
`pinned` exempts anything worth keeping. A pinned artifact keeps its bytes even after its task's
transcript is gone, and so does one belonging to a pinned task.

Removing the row is not permission to remove the file: two rows can name one digest, so the sweep
asks which digests nothing references any more, in the transaction that deletes, and only those
leave the disk.

## 14. Stack

Versions below are what the repository actually builds with, read from `go.mod`, `package.json` and
`components.json` on 2026-08-25, not a plan.

### Go

The dependency list is deliberately close to empty: two non-stdlib imports, and neither has a
stdlib equivalent.

| Component | Version | Path | Note |
|---|---|---|---|
| toolchain | **1.27.0** | (none) | `go.mod` names it directly; needs macOS 13+ |
| router | stdlib | `net/http` | Go 1.22+ method+wildcard patterns are enough; no third-party router |
| sqlite | v1.57.0 | `modernc.org/sqlite` | pure Go, so `CGO_ENABLED=0` and a static binary |
| websocket | v1.8.15 | `github.com/coder/websocket` | the cockpit's live stream (§7.5); successor to `nhooyr.io/websocket`, gorilla is maintenance-only |
| embed | stdlib | `embed` | **must** be `//go:embed all:dist`, since a plain directive skips Vite's `dist/.vite/` and the page fails to load with a successful build |

Everything else, meaning process supervision, the event bus, the unix socket and the agent client, is
stdlib. `os/exec`, `net`, `encoding/json`, `log/slog`.

**Planned, not yet present.** `github.com/creack/pty` is named in §10.1 and is not in `go.mod`,
because terminal takeover is not built. It is listed here so the gap is visible rather than
discovered.

### Frontend

| Component | Version | Note |
|---|---|---|
| Vue | 3.5.41 | 3.6 (Vapor) is RC, so do not let `@next` in |
| Vite | 8.2.2 | Rolldown is default, ESM-only, Node 20.19+/22.12+ |
| shadcn-vue | 2.8.2 | CLI; `v3.shadcn-vue.com` is an **archived docs site**, not a package line |
| reka-ui | 2.10.3 | the primitive layer; `radix-vue` was renamed to this and is frozen at 1.9.17 |
| Tailwind | 4.3.3 | CSS-first: no `tailwind.config.js`; `@tailwindcss/vite` + `@theme {}` |
| TypeScript | **6.0.3, pinned** | TS 7 is the Go-native rewrite, shipping without a stable programmatic compiler API; `vue-tsc` is pinned to TS 6 until ~7.1. Its peer range says `>=5.0.0`, which is misleading. Verify before unpinning |
| vue-tsc | 3.3.11 | runs in `pnpm build` as `--noEmit`; `build:fast` skips it |
| Pinia | 4.0.3 | |
| vue-router | 5.2.0 | |
| @vueuse/core | 14.4.0 | |
| @lucide/vue | 1.33.0 | icon library |
| TipTap | 3.30.5 | `@tiptap/vue-3`, `-core`, `-pm`, `-starter-kit`, `-extensions`. The brief editor (§10.2.1) |
| tiptap-markdown | 0.9.0 | Markdown in and out of that editor. **Unmaintained by choice**: its author stopped at TipTap 3 because TipTap's own Markdown conversion became a paid extension. MIT, and a thin wrapper over `prosemirror-markdown`, which is the fallback if it ever breaks |
| JetBrains Mono | (none) | Google Fonts; the preset's face for headings *and* body |

**pnpm, not npm**, at 11.22.0. This is not a preference: pnpm 11 blocks postinstall scripts unless
a package is allowlisted, and `vue-demi` (which pinia and reka-ui both rely on) needs one. Without
the allowlist the install exits non-zero, and that non-zero exit is what makes `shadcn-vue init`
report a failed dependency step even though every package installed correctly. The allowlist lives
in `web/pnpm-workspace.yaml`, which is where pnpm 11 moved it from `package.json`.

**Not present:** `@xterm/xterm`. §10.1's terminal takeover is unbuilt, so the terminal *styling* in
the cockpit is CSS over ordinary elements, not a real emulator.

### 14.1 UI foundation

The cockpit was scaffolded with the shadcn-vue CLI:

```sh
pnpm dlx shadcn-vue@latest init --preset awGASPI --template vite
```

That resolves to style *reka-lyra*, JetBrains Mono, base color *mauve*, **radius 0**, Menu
Default/Subtle, and **lucide** icons, recorded in `web/components.json`, which is the file to read
rather than this paragraph if the two disagree.

Two rules about it:

- **Components in `src/components/ui/` are CLI output. Do not hand-write them.** Add with
  `pnpm dlx shadcn-vue@latest add <name>`. Hand-written lookalikes drift from the registry and
  silently miss the token wiring that makes theming work at all.
- **Chart and tier colors are not decorative and must not be re-picked by eye.** `--chart-1..4`
  (role identity) and `--tier-1..4` (the cost ramp of §11.4) live in
  [`web/theme.css`](web/theme.css) and were validated against the dark surface, worst adjacent CVD
  ΔE 9.4. All four chart hues sit at L≈62%, which is *why* they pass. Changing one means re-running
  the validator, not nudging a hex.

One standing cost to watch: **mono for body text** is the preset's call. It suits dense tool UI,
meaning tables, logs and the activity view, and it is why the terminal skin and the cockpit chrome feel
continuous. It is less comfortable for long prose, so watch the one screen that has any, the prompt
editor. If it reads tiring at length, that editor is the place to make an exception, not the app.

> **Node floor:** the shadcn-vue CLI and Vite 8 require Node `^22.18.0 || >=24.12.0`. This machine's
> bare shell resolves to v22.12.0 while nvm's default alias is v24.19.0, so tooling must run under
> 24.19.0. `.nvmrc` pins it rather than trusting ambient shell state, the same version-skew class
> broke `pi` locally.

---

## 15. Build order, as built

The order below is what was actually followed, and milestones 1–2 being LLM-free is the reason §6
and §6.1 are almost entirely coordination bugs caught without spending a token.

1. **store + role library + project team API**: templates as rows with the eight built-ins seeded.
2. **nydus + board** against an in-memory harness stub: leases, claims, acks and terminal merge
   proven with zero LLM calls.
3. **adapter interface + claude adapter + preflight**: a team that cannot work never reaches a
   running board.
4. **cerebrate** supervision, lease expiry, crash/backoff.
5. **Cockpit v1**: projects, team, role editor, attention, board.
6. **pi adapter**: the second adapter is what turned the interface from a description of claude
   into a contract; three fields the design required were unexercised until then.
7. **Cost accounting and event replay**: usage per turn, not per run.
8. **WebSocket transport** (§7.5), replacing the SSE stream it started as.
9. **Approval gates, integration modes and the gate diff** (§9.1, §9.2): the run that produced
   §6.1 is what motivated showing `base...sha` at the gate that performs the merge.
10. **Tailnet and TLS** (§17), because a board you cannot read from a phone gets read less.
11. **Provider-limit handling** (§16): a spent quota window pauses a role
    instead of failing it.
12. **Process continuation** (§7.4): `--detach`, a swarm that comes back after a daemon restart,
    and agents that resume the conversation they were holding rather than respawning cold. The
    money argument that had kept a swarm stopped is answered by the operator's own intent: only a
    project somebody started and has not stopped comes back, and there is a switch for the people
    that is not enough for.

Still open: pty attach and takeover (§10.1, needs `github.com/creack/pty`), deploying an artifact
somewhere (issue #9's second half, which has four open questions in it and no answers yet), and
authentication (§17).

---

## 16. Provider limits

A subscription window has two questions: how much is left, and what happens
when it runs out. §16.1 is the first; §16.2 onward is the second.

A spent window looks exactly like a fatal error and is not
one. Nothing is wrong with the agent, the code or the task, and the correct
response is to wait, so treating it as a crash costs an operator the twenty
minutes it takes to discover that the thing to do was nothing.

### 16.1 Where the numbers come from

Two different mechanisms, because the harnesses differ:

| | how the gauge is obtained | cost |
|---|---|---|
| `claude` | a `rate_limit_event` on **every turn** of `--output-format stream-json`, which zerg already reads | free, nothing to poll |
| `pi` | `GET https://chatgpt.com/backend-api/wham/usage`, with the OAuth token pi stores, polled every two minutes | one request per two minutes |

claude's event carries `unifiedWindows` keyed `five_hour` and `seven_day`, each
with a `utilization` (0..1) and a `resetsAt`. It exists only in the streaming
format: `--output-format json` collapses to the final result and drops it, which
is why a first pass through the CLI concluded, wrongly, that nothing was
available. `claude -p "/usage"` also works non-interactively and costs nothing,
zero turns and zero tokens, but it is a second path to the same numbers, so it is
not used.

pi has no such signal: its own `/usage` reports session tokens, not plan
headroom. The endpoint above is what the ChatGPT app's own meter uses, and the
request shape was read from the `pi-chatgpt-limit` extension
(github.com/patlux/pi-chatgpt-limit), which does the same thing inside pi. It is
undocumented, so every failure is soft: a gauge that cannot be read must never
stop a role from running, and the last good reading is kept with its timestamp
rather than blanked.

**The gauge is keyed by provider, not harness.** A ChatGPT window says nothing
about the deepseek key beside it, and pi fronts both; equally, two harnesses
authenticated to one account should show one gauge rather than double-report it.
Harness is only the fallback when an adapter does not name a provider.

It lives in the sidebar, under the roles that spend it, rather than the top bar.
On a phone that bar already carries the project, the alerts and the run control,
and a gauge is something you go and look at rather than something that has to be
in your eye.

**Windows are identified by length, not name.** claude names them; the ChatGPT
endpoint returns a `primary_window` and a `secondary_window` with only a
`limit_window_seconds` each, and on a live account the *primary* was the 7-day
one. Position and name are both unreliable; a duration means the same thing to
both.

Only the tightest window is coloured. It is the one that will actually stop
work, and two coloured bars would say the same thing twice.

### 16.2 Throttling is a state, not a failure

`StateThrottled` is distinct from `StateFailed` because it needs nobody to do
anything: it ends by itself. The supervisor holds the role until the window
rolls over, a minute past the stated time, since resuming exactly on it races
the provider's clock and losing that race spends another attempt to learn
nothing, then respawns and resets the backoff. The worktree and configuration
are untouched, so this is a pause rather than a teardown.

When the harness says the window is spent but not when it lifts, which is common, the
role rechecks every five minutes rather than guessing an end. A role that
announces it resumes at a time it will not is worse than one that says it does
not know.

The check runs on **every** failed run, not only fatal ones. The two harnesses
disagree about severity: pi's quota error is fatal, claude's decodes as an
ordinary error, and a first version that checked only the fatal path left claude
crash-looping with exponential backoff through a limit it should have waited
out. The agent's own output is consulted before the process exit status, because
an exit status says a process died, not that a provider refused it.

Detection is `adapter.Throttler`, an **optional** interface reached by type
assertion. A harness that cannot tell a quota limit from any other failure
should not be forced to pretend, and the assertion keeps every existing
implementation and test double compiling.

The matching is deliberately narrow. claude's binary carries *"Lower-priority
mode is offered again after your weekly limit resets"*, which is informational,
matching a bare `limit resets` would pause a working agent, which is a far worse
failure than missing a throttle. The blocking phrase is matched in full, and a
test asserts the informational one does not trigger.

The cockpit shows amber, not red, with the one fact the state raises: *provider
limit · resumes in 47m*. Red would send someone looking for a problem to fix.

---

## 17. Network exposure

The daemon binds `127.0.0.1:7717` by default. `--addr`, or **Settings → Network**, binds one other
interface. Over Tailscale, that is the tailnet address.

**TLS has three modes.** `off`; `tailscale`, which asks the local `tailscaled` for a Let's Encrypt
certificate for this machine's MagicDNS name; and `files`, for a certificate you supply. The
Tailscale path needs **HTTPS Certificates** enabled for the tailnet in the admin console, and the
settings view says so when it is not.

Tailscale is reached through its **CLI**, not `tsnet`. Measured: `tsnet` pulls 547 modules and
about 30 MB into a project whose entire dependency set is otherwise two modules and 18 MB. The
daemon shells out to a `tailscaled` that is already running for other reasons.

**Loopback keeps working alongside it.** A second listener serves plain HTTP on `127.0.0.1` on the
same port: one daemon, two doors. Local work does not need the MagicDNS name, and a network
setting that breaks the main listener cannot lock you out of the view that would fix it.

**Cross-origin writes are refused.** A page you visit can resolve its own
hostname to `127.0.0.1` and then post to this API from your browser, which is DNS
rebinding, and the tailnet cannot help, because the request originates inside
it. That matters more here than in most apps: agents run with permission
prompts disabled, in worktrees of repositories you chose, so *create a task* is
arbitrary code execution on this machine. Mutating requests must carry a
`Sec-Fetch-Site` of `same-origin`/`none`, or an `Origin` matching the `Host`.
Reads are untouched, and a client that sends neither header, such as curl or a script,
is allowed, because it is not what this defends against. The WebSocket has
always enforced the same-origin check its library provides.

Bodies are capped at 1 MB, and the server has `ReadTimeout` and `IdleTimeout`
as well as `ReadHeaderTimeout`. There is deliberately no `WriteTimeout`: the
activity stream is a long-lived WebSocket and a server write deadline would cut
it, so the stream sets its own per-write deadline instead.

**There is still no authentication.** Anything that can route to the port can start agents, read every
transcript, and see which repositories are being worked on. On a tailnet that is your own devices,
which is the point; `--addr 0.0.0.0` hands it to whatever else shares the local network. The daemon
states which of the two you have chosen at startup, and does not pretend the difference is small.
