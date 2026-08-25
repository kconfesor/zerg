<p align="center">
  <img src="web/public/logo-full.svg" alt="zerg logo" width="540">
</p>

<p align="center">
  <b>Multi-agent coding orchestrator</b><br>
  A Go daemon supervises a team of agent harnesses working in isolated git worktrees, routes work between them, and serves a Vue 3 cockpit.
</p>

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
- **your plan's headroom is on screen** — claude reports it on every turn, pi is asked for it; a
  bar per window, coloured only where it is nearly spent
- **a spent quota is a pause, not a crash** — a role that hits its subscription limit waits for the
  window and resumes itself, saying when ([§16](ARCHITECTURE.md#16-provider-limits))
- **one SQLite database**, one writer, real transactions
- **nothing reports success it did not observe** — see [§6.1](ARCHITECTURE.md#61-what-the-first-real-run-broke),
  which is the record of a task reaching Done over a branch that had never moved
- **it is a guest in your repository** — prompts are appended, never substituted, so your
  `CLAUDE.md` and `AGENTS.md` still apply; nothing is written into the tree but `.worktrees/`, and
  that ignore rule goes in `.git/info/exclude` rather than your `.gitignore`
  ([§4.4.1](ARCHITECTURE.md#441-what-zerg-injects-and-what-it-leaves-alone))

Provider setup is out of scope: log into `pi` and `claude` yourself. zerg detects credential state
and tells you what to fix — it never runs a login flow or touches an auth file.

## Approving, and where work lands

A role can be gated. A gated handoff is held before the next role sees it; a gated **last** role is
the approval that performs the merge, and it shows `git diff base...sha` — everything the task would
land, across every role and every lap, not just the final commit.

Per project you choose what that approval does: fast-forward the base branch, open a pull request
with `gh`, or leave the work on its branch. It is a project setting rather than a role setting
because only the last role integrates, and which role that is changes when you change the team.

Finished cards can be put away one at a time; the switch to show them again is off by default.

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
  chat/            ask the repository a question, outside the pipeline
  store/           sqlite (~/.zerg/zerg.db), migrations, role library
  event/           typed event bus, logging and usage recording
  preflight/       readiness checks run before anything spawns
  api/             http, serves the embedded cockpit
  hatchery/        workspace and worktree management
  tailnet/         tailscale status and certificates, via its CLI
web/               Vue 3 + Vite + shadcn-vue cockpit
```

## Stack

Go 1.27 · stdlib `net/http` · modernc.org/sqlite (cgo-free, so `CGO_ENABLED=0` and a static binary) · coder/websocket
Vue 3.5 · Vite 8 · shadcn-vue 2.8 (reka-ui) · Tailwind 4 · TypeScript 6 (pinned) · pnpm

`modernc.org/sqlite` and `coder/websocket` are the only non-stdlib Go dependencies. Pinned versions and their gotchas:
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

### From a phone

The cockpit is responsive: below 768px the nav becomes a drawer, board lanes
stack, and the activity stream puts its timestamps above each line rather than
beside them.

It binds to loopback by default. To reach it from another device, bind the one
interface you want — over Tailscale, that is the tailnet address:

Set it in **Settings → Network**, or for one run:

```sh
./zerg up --addr $(tailscale ip -4):7717
```

Turn on **TLS → Tailscale certificate** and zerg asks the local tailscaled for a
real Let's Encrypt certificate for this machine's MagicDNS name, so a phone gets
no warning. It needs **HTTPS Certificates** enabled for the tailnet, under DNS in
the admin console; the settings view says so when it is off.

`localhost` keeps working alongside it. A second listener serves plain HTTP on
loopback on the same port — one daemon, two doors — so local work does not need
the MagicDNS name, and a network setting that breaks the main listener cannot
lock you out of the view that sets it.

**The cockpit has no authentication.** Anything that can route to that port can
start agents, read every transcript, and see which repositories are being worked
on. On a tailnet that is your own devices, which is the point; `--addr 0.0.0.0`
also hands it to whatever else shares the local network. The daemon says which
of the two you have chosen at startup.

## Tests

```sh
go test ./internal/...
```

The coordination layer is testable without spending a token, and is tested that way. Tests that
assert an effect check the system that was supposed to change — git, the database — rather than
reading back a field the code set. [§6.1](ARCHITECTURE.md#61-what-the-first-real-run-broke) is what
happens when they don't.
