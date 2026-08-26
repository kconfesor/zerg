<p align="center">
  <img src="web/public/logo-full.svg" alt="zerg logo" width="540">
</p>

<p align="center">
  <b>Multi-agent coding orchestrator</b><br>
  A Go daemon supervises a team of agent harnesses working in isolated git worktrees, routes work between them, and serves a Vue 3 cockpit.
</p>

**Everything is configured in the UI.** No config files or prompt files to copy. Define reusable
teams once, clone one when a project needs different settings, and choose which team each project uses.

Status: **running.** Coordination, harnesses, preflight, board and cockpit are implemented and have
completed real tasks end to end. See [ARCHITECTURE.md](docs/ARCHITECTURE.md).

```sh
git clone https://github.com/kconfesor/zerg.git && cd zerg
./build.sh && ./zerg up          # http://127.0.0.1:7717
```

Two things to know before you point it at a repository. **It runs agent CLIs as child processes**,
with your permissions, doing whatever the task requires — the same thing you accept by running
`claude` or `pi` yourself, several at a time and unattended. And **the API has no authentication**:
bind it to loopback or a Tailscale tailnet and nothing else. [SECURITY.md](SECURITY.md) is the
short version of what is and is not defended.

| | |
|---|---|
| [Set it up](#setting-up-on-a-new-machine) | toolchain, harness login, first project |
| [Reach it from a phone](#reaching-it-from-a-phone) | Tailscale, TLS, installing it as an app |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it works, and the failures behind each decision |
| [CONTRIBUTING.md](CONTRIBUTING.md) | building, testing, and the traps in this codebase |
| [SECURITY.md](SECURITY.md) | threat model, and reporting a vulnerability |

## Why

Agents in separate git worktrees handing each other committed SHAs is the right shape for this
problem. Coordination and configuration are where it goes wrong, and both fail quietly.

Two incidents set the design. A day of running an earlier build produced four hangs that all
presented identically — an agent that looks alive and does nothing — every one of them detectable
before spawning. And a task was silently built in the wrong language, because config had been
snapshotted into the worktrees and a later edit reached no one.

So:

- **configure in the UI** — roles, reusable teams, harnesses, models and prompts are database rows,
  not files; prompts are composed fresh at every spawn
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
  window and resumes itself, saying when ([§16](docs/ARCHITECTURE.md#16-provider-limits))
- **one SQLite database**, one writer, real transactions
- **nothing reports success it did not observe** — see [§6.1](docs/ARCHITECTURE.md#61-what-the-first-real-run-broke),
  which is the record of a task reaching Done over a branch that had never moved
- **it is a guest in your repository** — prompts are appended, never substituted, so your
  `CLAUDE.md` and `AGENTS.md` still apply; nothing is written into the tree but `.worktrees/`, and
  that ignore rule goes in `.git/info/exclude` rather than your `.gitignore`
  ([§4.4.1](docs/ARCHITECTURE.md#441-what-zerg-injects-and-what-it-leaves-alone))

Provider setup is out of scope: log into `pi` and `claude` yourself. zerg detects credential state
and tells you what to fix — it never runs a login flow or touches an auth file.

## Approving, and where work lands

A role can be gated. A gated handoff is held before the next role sees it; a gated **last** role is
the approval that performs the merge, and it shows `git diff base...sha` — everything the task would
land, across every role and every lap, not just the final commit.

Per project you choose what that approval does: fast-forward the base branch, open a pull request
with `gh`, or leave the work on its branch. Pull requests can be opened ready for review or as drafts.
It is a project setting rather than a role setting because only the last role integrates, and which
role that is changes when you change the team.

Finished cards can be put away one at a time; the switch to show them again is off by default.

## Roles and teams

**A role is a worker.** Not a job title — an actual agent process, with its own git worktree, its
own harness (`claude`, `pi`), its own model, its own prompt, and its own inbox. `reviewer` means "an
agent running opus in `.worktrees/reviewer`, carrying the reviewer prompt, that holds its handoffs
for approval". Nothing about a role is a hint to the model; every field is what the daemon actually
executes.

**A team is an ordered pipeline of roles.** It picks roles out of the library, orders them, and
enables or disables each one. Work enters at the first enabled role and is handed down: each role
commits in its worktree and passes the SHA to the next, so the role below always starts from
committed code rather than a description of it. The last enabled role is the one that integrates
(see above) — which role that is changes when you reorder or disable, and the board's Done lane is
what comes after it.

A library of eight roles ships — planner, coder, reviewer, cleaner, architect, hardener, security,
docs — plus a **Default** team of `coder` (sonnet) → `reviewer` (opus).

### Four layers, and where each one is edited

Settings apply in this order, and each layer only has to say what differs from the one above it:

| Layer | Where | What it decides |
|---|---|---|
| **Role library** | Settings → Roles | What a role *is* — its harness, model, prompt, gate. Global. Editing one here changes the default for every team that uses it. |
| **Team** | Team → *select a team* | Which roles are on it, in what order, enabled or not — and any field it wants to specialize for this team's purposes. |
| **Project** | Team → *the project's team* | The same fields again, for one repository only. Optionally the pipeline itself, which then stops following the team's. |
| **Runtime** | — | Tasks, leases, messages, cost. Never configuration. |

Null means inherit, a value means local, at every layer. A role that differs from what it inherited
is badged in the team list, so a project that drifted from its team is visible rather than a
surprise. Empty arguments are a value, not a blank: it means *run with no arguments*, which is
different from *inherit the arguments*.

Which is why the library and the team editor are separate screens. Renaming a prompt in the library
is a decision about every project on this machine; checking a role onto a team is a decision about
one pipeline. Doing both in one place is how a fix to one project quietly changes another.

### Working with them

Teams are shared, so editing one edits it everywhere it is used. Selecting a team in the list only
opens it for editing — **Use this Team** is the separate button that assigns it — so browsing never
changes what runs, and the view warns you when the team on screen is not the one this project is on.
It warns harder while agents are running, because a pipeline change applies immediately. When a
project needs something different, **Clone** the team, change the clone, then assign it.

Deleting a role from the library takes it off every team that had it, so the confirmation names
them first.

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
[ARCHITECTURE.md §14](docs/ARCHITECTURE.md#14-stack).

## Setting up on a new machine

Four steps. The first three are things zerg checks and reports on rather than does for you — it
never runs a login flow or writes to an auth file.

### 1. Toolchain

| | Version | Why |
|---|---|---|
| **Go** | 1.27+ (`go.mod`) | builds the daemon; `CGO_ENABLED=0` works, the SQLite driver is pure Go |
| **Node** | 24.19.0 (`.nvmrc`) | builds the cockpit. Vite 8 needs `^22.18.0 \|\| >=24.12.0` |
| **pnpm** | 11 | `pnpm-lock.yaml` is the lockfile; `npm` will not reproduce it |
| **git** | any recent | worktrees are the isolation mechanism |

```sh
go version && node -v && pnpm -v && git --version
```

`build.sh` checks Node itself and refuses with the version it wanted, because a build that silently
runs on the wrong one fails much further downstream.

Optional: **`gh`**, only if a project integrates by opening a pull request. Merge and branch modes
never call it.

That is the whole list. No tmux, no babashka, no zsh — agents are child processes of the daemon
([§7.4](docs/ARCHITECTURE.md#74-no-tmux)), so there is no session manager to install or attach to.

### 2. Log a harness in

At least one, and zerg will not do it for you:

```sh
claude          # then /login, and answer the trust prompt in any repo you will use
pi              # then /login for the provider whose models you plan to select
```

zerg reads credential state and reports it; it never runs a login flow or touches an auth file.
Readiness will tell you exactly which of these is missing per role — see step 4.

### 3. Build and run

```sh
./build.sh          # cockpit → web/dist → embedded in the binary → ./zerg
./zerg up           # 127.0.0.1:7717
```

Or by hand, which is what `build.sh` does:

```sh
pnpm --dir web install --frozen-lockfile && pnpm --dir web build
rm -rf internal/api/dist && cp -R web/dist internal/api/dist   # go:embed cannot reach outside its package
go build -o zerg ./cmd/zerg
```

State lives in `~/.zerg/zerg.db` (override with `--db`), which is created on first run along with
the eight built-in role templates. The directory and the database are `0700`/`0600` — they hold every
prompt, transcript and cost this machine has produced.

```
zerg up [--addr host:port] [--no-tls] [--db path] [--verbose]
```

`--addr` and `--no-tls` override the stored settings for one run. `--no-tls` is the way back in if a
TLS setting turns out not to be satisfiable: without it, saving one can lock you out of the settings
view that sets it.

### 4. Point it at a repository

In the cockpit: **Projects → Add a project** (an absolute path, checked to exist and be a
directory), then **Team** to select or customize a reusable team, then **Readiness**. On the first
Start, a directory that is not a repository yet gets `git init` and one commit, because a worktree needs history to branch
from — that is the only commit zerg authors rather than an agent.

Readiness is the step worth not skipping, though you cannot really skip it: **Start refuses while
any enabled role is blocked**, and the refusal carries the report, so pressing it anyway lands you
on this screen. A team that cannot work must never reach a running board. It runs every check for
every enabled role and states a remedy for each failure:

| Check | What it catches |
|---|---|
| `binary_present` | the harness is not on PATH |
| `binary_version` | the CLI does not answer `--version` |
| `config_parses` | the CLI's own config is corrupt — two agents racing a read-modify-write once left one holding three concatenated copies of itself |
| `credentials` (pi) | no credential for the selected provider → *run pi and use /login for that provider* |
| `workspace_trusted` (claude) | the trust prompt was never answered for that directory |
| `model_available` | the model id is not in the harness's catalog |
| `extensions_loadable` (pi) | every extension failed to load, usually a Node version mismatch |

## Reaching it from a phone

The cockpit binds to `127.0.0.1:7717` and is responsive: below 768px the nav becomes a drawer, board
lanes stack, dialogs go full screen, and the top bar names the project beside the agent count.

### Check Tailscale first

```sh
tailscale status     # logged in, and what this machine is called
tailscale ip -4      # the address to bind to
```

zerg asks the same daemon the same questions (`tailscale status --json`), and reports the answer in
**Settings → Network** — so you can skip this and read it there. On a fresh machine the command is
faster, and four things can be wrong:

| Symptom | Meaning | Fix |
|---|---|---|
| `tailscale: command not found` | not installed | [tailscale.com/download](https://tailscale.com/download) |
| `Logged out` or a connection error | tailscaled is not running, or this machine is logged out | `tailscale up` |
| no MagicDNS name in `status` | MagicDNS is off | enable **MagicDNS** under DNS in the admin console |
| `tailscale cert <name>` fails | HTTPS certificates are off for the tailnet | enable **HTTPS Certificates** under DNS in the admin console |

Only the last one is specific to TLS; the first three are needed to reach the cockpit at all.
Everything here degrades rather than fails — a machine without Tailscale is the normal case, and
zerg says "not available" rather than erroring.

### Bind and secure it

In **Settings → Network**, set the address to the tailnet IP and TLS to **Tailscale certificate**.
For one run instead:

```sh
./zerg up --addr $(tailscale ip -4):7717
```

With TLS set to `tailscale`, zerg asks the local tailscaled for a real Let's Encrypt certificate for
this machine's MagicDNS name, so a phone gets no warning. `tailscale cert` is idempotent and renews
in place, so this happens on every start and reuses a valid certificate. Certificates land in
`~/.zerg/state/certs/`.

The alternative is TLS **files**, pointing at a certificate and key you already have.

`localhost` keeps working alongside it: a second listener serves plain HTTP on loopback on the same
port — one daemon, two doors — so local work does not need the MagicDNS name, and a network setting
that breaks the main listener cannot lock you out of the view that sets it. Turn it off with
**Local access** if you would rather it did not.

Address and TLS changes apply on restart, and the settings view says so; retention and cleanup apply
immediately. The daemon prints the URL at startup, and both of them once there are two:

```
Cockpit: https://your-machine.tailXXXX.ts.net:7717
Locally: http://127.0.0.1:7717
note: reachable at 100.x.y.z:7717 beyond this machine, with no authentication.
      Treat anything that can route to it as trusted.
```

> **The cockpit has no authentication.** Anything that can route to that port can start agents, read
> every transcript, and see which repositories are being worked on. On a tailnet that is your own
> devices, which is the point; `--addr 0.0.0.0` also hands it to whatever else shares the local
> network. The daemon says which of the two you have chosen at startup.

### Installing it as an app

The cockpit ships a web manifest with `display: standalone`, so **Add to Home Screen** on iOS or
**Install** on Android gives it its own window with no browser chrome. Dialogs account for the
notch, spanning the safe-area insets rather than the raw viewport.

## Running it unattended

`zerg up` runs in the foreground and its agents are its children, so closing the terminal stops
everything. There is no `--detach` yet; use a launchd or systemd unit, or `nohup`.

A restart is a first-class path, not a recovery hack: every open lease is reclaimed immediately
rather than left to lapse, and an approval interrupted mid-integration is settled against the
repository — merged means the decision is recorded and the card closed, not merged means it returns
to you as pending. Swarms do not resume by themselves, which is deliberate while spawning an LLM
process costs money.

## Tests

```sh
go test ./internal/...            # coordination, routing, store, adapters
pnpm --dir web test               # the cockpit's logic: arg round-trips, stale-response guards, combobox keys
```

Neither spends a token. The coordination layer is testable without one, and is tested that way. Tests that
assert an effect check the system that was supposed to change — git, the database — rather than
reading back a field the code set. [§6.1](docs/ARCHITECTURE.md#61-what-the-first-real-run-broke) is what
happens when they don't.

## Scope

What this is not, so you can decide quickly whether it is for you:

- **Not a hosted service.** One daemon, one machine, your repositories, your provider accounts.
  There is no multi-tenancy and no account system, which is why there is no authentication either.
- **Not a model provider.** zerg drives coding CLIs you have already logged into. It never runs a
  login flow, never writes an auth file, and cannot spend on an account you have not set up.
- **Not an autonomous engineer.** It routes work between agents and holds the gates where you asked
  for one. What lands is what a role committed and what you approved.
- **Not stable yet.** `main` is the only branch, the database migrates forward in place, and
  nothing here is versioned for compatibility.

## Contributing

Bugs, ideas and pull requests are welcome — [CONTRIBUTING.md](CONTRIBUTING.md) covers building it,
testing it, and the handful of things in this codebase that bite (the committed cockpit, append-only
migrations, and why `DROP TABLE` is never the answer). Security issues go
[privately](SECURITY.md).

## Licence

[Apache License 2.0](LICENSE). Copyright 2026 Kelvin Confesor.
