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
with your permissions, doing whatever the task requires. It is the same thing you accept by running
`claude` or `pi` yourself, several at a time and unattended. And **the API has no authentication**:
bind it to loopback or a Tailscale tailnet and nothing else. [SECURITY.md](SECURITY.md) is the
short version of what is and is not defended.

| | |
|---|---|
| [What it looks like](#what-it-looks-like) | screenshots, before you install anything |
| [Set it up](#setting-up-on-a-new-machine) | toolchain, harness login, first project |
| [Which harness, and which model where](#which-harness-and-which-model-where) | pi and claude, and why the reviewer should not be the coder |
| [Reach it from a phone](#reaching-it-from-a-phone) | Tailscale, TLS, installing it as an app |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it works, and the failures behind each decision |
| [CONTRIBUTING.md](CONTRIBUTING.md) | building, testing, and the traps in this codebase |
| [SECURITY.md](SECURITY.md) | threat model, and reporting a vulnerability |
| [Roadmap](#roadmap) | what is next, what is later, what is undecided |

## Why this exists

I got tired of looking at terminal screens.

Running several agents means several sessions, and the state of the work ends up scattered across
them: which one is waiting on me, which one has been stuck for twenty minutes, what the last one
actually committed, what any of it has cost. Answering that meant switching panes and reading
scrollback, and the answer went stale as soon as I looked away.

The part that decided it was the phone. Following work from a terminal on a phone is a mess: tiny
text, no window management, scrollback you cannot skim, and a session that drops when the screen
locks. But a run does not stop needing you when you leave the desk. A gated handoff waits, a role
that hit a requirement it could not guess waits, and both of them wait until you are back in the
room with the machine.

So the state lives in a database and is served as a screen, not a scrollback: what each role is
doing, which card is where, what it has spent, and what needs a decision. It is built for a phone
first because that is where I read it from, which is why [setting up Tailscale](#reaching-it-from-a-phone)
is not optional here so much as the point.

## What it looks like

A demo project, `swarm-sim`, with a four-role pipeline: `planner` on opus, `coder` and `docs` on
sonnet, and `reviewer` on pi with an OpenAI model, because
[the reviewer should not be the coder](#do-not-review-your-own-work). The planner and the reviewer
are gated, so both ends of the pipeline stop for a person.

**The board.** One lane per role, plus Done. Every card carries what it is waiting on, how long it
has been going, and what it has cost.

![The board, with cards in four lanes](docs/screenshots/01-board.png)

**What is waiting on you.** Approvals and questions in one place, over whatever you were reading.
The gated diff is `git diff base...sha`: everything the task would land, across every role and every
lap, not just the final commit.

![An approval with its diff expanded, and a question from the coder](docs/screenshots/02-attention.png)

**Where a card has been.** Every handoff as a sequence, with the note each role wrote and the commit
it pointed at. Rejections point back up the pipeline, so a card that bounced looks like one.

![A finished card's history as a sequence diagram](docs/screenshots/03-card-flow.png)

**What the agents are doing.** Not a captured terminal: every line is a typed event, so it filters
by role, survives a reload, and reads back later.

![The activity stream, filtered by role](docs/screenshots/06-activity.png)

**What it cost.** Per role and per provider, with input split into its three classes, because cached
and uncached tokens differ by roughly 50x in price and one blended number hides the only lever you
have. Subscription rows are labelled as estimates rather than presented as a bill.

![Spend by role and provider](docs/screenshots/05-spend.png)

**The team.** Roles on the left, what each one runs on in the middle, the order on the right.
Changing a team changes it everywhere it is used, which the view says out loud.

![The team editor](docs/screenshots/04-team.png)

**Settings.** The role library, where a role's harness, model, prompt and gate are defined once for
every team that uses it, alongside network and TLS, disk and retention, harness flags, and the
shared instructions every role is given.

![Settings](docs/screenshots/07-settings.png)

**On a phone**, which is the case this was designed around: the nav becomes a drawer, lanes stack,
and dialogs go full screen.

<p>
  <img src="docs/screenshots/08-phone-board.png" alt="The board on a phone" width="270">
  <img src="docs/screenshots/09-phone-attention.png" alt="An approval on a phone" width="270">
</p>

> These are a demo database against a toy repository, so nothing here is a benchmark: the numbers
> are what that data says, not a claim about how fast or cheap your pipeline will be.

## Why it is built this way

Agents in separate git worktrees handing each other committed SHAs is the right shape for this
problem. Coordination and configuration are where it goes wrong, and both fail quietly.

Two incidents set the design. A day of running an earlier build produced four hangs that all
presented identically, as an agent that looks alive and does nothing, and every one of them detectable
before spawning. And a task was silently built in the wrong language, because config had been
snapshotted into the worktrees and a later edit reached no one.

So:

- **configure in the UI**: roles, reusable teams, harnesses, models and prompts are database rows,
  not files; prompts are composed fresh at every spawn
- **harnesses are adapters**, not a hardcoded switch: `claude` and `pi` first
- **model pickers from the harness's own catalog**, so you stop typing model ids that 400
- **preflight before spawn**: a stale CLI, a corrupt config, an unanswered trust dialog or a broken
  plugin tree becomes a visible blocked role with a stated remedy
- **agents emit structured events**, so the cockpit renders tool calls, tokens and cost instead of
  scraping a terminal
- **leases, not fire-and-forget**: unacked work returns to the queue, and a stall is a state rather than silence
- **your plan's headroom is on screen**: claude reports it on every turn, pi is asked for it, with a
  bar per window, coloured only where it is nearly spent
- **a spent quota is a pause, not a crash**: a role that hits its subscription limit waits for the
  window and resumes itself, saying when ([§16](docs/ARCHITECTURE.md#16-provider-limits))
- **one SQLite database**, one writer, real transactions
- **nothing reports success it did not observe**. See [§6.1](docs/ARCHITECTURE.md#61-what-the-first-real-run-broke),
  which is the record of a task reaching Done over a branch that had never moved
- **it is a guest in your repository**: prompts are appended, never substituted, so your
  `CLAUDE.md` and `AGENTS.md` still apply; nothing is written into the tree but `.worktrees/`, and
  that ignore rule goes in `.git/info/exclude` rather than your `.gitignore`
  ([§4.4.1](docs/ARCHITECTURE.md#441-what-zerg-injects-and-what-it-leaves-alone))

Provider setup is out of scope: log into `pi` and `claude` yourself. zerg detects credential state
and tells you what to fix. It never runs a login flow or touches an auth file.

## Approving, and where work lands

A role can be gated. A gated handoff is held before the next role sees it; a gated **last** role is
the approval that performs the merge, and it shows `git diff base...sha`: everything the task would
land, across every role and every lap, not just the final commit.

Per project you choose what that approval does: fast-forward the base branch, open a pull request
with `gh`, or leave the work on its branch. Pull requests can be opened ready for review or as drafts.
It is a project setting rather than a role setting because only the last role integrates, and which
role that is changes when you change the team.

Finished cards can be put away one at a time; the switch to show them again is off by default.

## Roles and teams

**A role is a worker.** Not a job title, but an actual agent process, with its own git worktree, its
own harness (`claude`, `pi`), its own model, its own prompt, and its own inbox. `reviewer` means "an
agent running opus in `.worktrees/reviewer`, carrying the reviewer prompt, that holds its handoffs
for approval". Nothing about a role is a hint to the model; every field is what the daemon actually
executes.

**A team is an ordered pipeline of roles.** It picks roles out of the library, orders them, and
enables or disables each one. Work enters at the first enabled role and is handed down: each role
commits in its worktree and passes the SHA to the next, so the role below always starts from
committed code rather than a description of it. The last enabled role is the one that integrates
(see above). Which role that is changes when you reorder or disable, and the board's Done lane is
what comes after it.

A library of eight roles ships (planner, coder, reviewer, cleaner, architect, hardener, security,
docs) plus a **Default** team of `coder` (sonnet) then `reviewer` (opus).

### Four layers, and where each one is edited

Settings apply in this order, and each layer only has to say what differs from the one above it:

| Layer | Where | What it decides |
|---|---|---|
| **Role library** | Settings → Roles | What a role *is*: its harness, model, prompt, gate. Global. Editing one here changes the default for every team that uses it. |
| **Team** | Team → *select a team* | Which roles are on it, in what order, enabled or not, plus any field it wants to specialize for this team's purposes. |
| **Project** | Team → *the project's team* | The same fields again, for one repository only. Optionally the pipeline itself, which then stops following the team's. |
| **Runtime** | (nowhere) | Tasks, leases, messages, cost. Never configuration. |

Null means inherit, a value means local, at every layer. A role that differs from what it inherited
is badged in the team list, so a project that drifted from its team is visible rather than a
surprise. Empty arguments are a value, not a blank: it means *run with no arguments*, which is
different from *inherit the arguments*.

Which is why the library and the team editor are separate screens. Renaming a prompt in the library
is a decision about every project on this machine; checking a role onto a team is a decision about
one pipeline. Doing both in one place is how a fix to one project quietly changes another.

### Working with them

Teams are shared, so editing one edits it everywhere it is used. Selecting a team in the list only
opens it for editing, and **Use this Team** is the separate button that assigns it, so browsing never
changes what runs, and the view warns you when the team on screen is not the one this project is on.
It warns harder while agents are running, because a pipeline change applies immediately. When a
project needs something different, **Clone** the team, change the clone, then assign it.

Deleting a role from the library takes it off every team that had it, so the confirmation names
them first.

## Which harness, and which model where

Both harnesses are first-class and the adapter interface exists so neither is special. They are not
the same tool, though, and the choice is worth making deliberately.

**pi** is excellent: efficient, plain, and quick to get out of your way. It is the one I reach for
by default, and it lets you run **OpenAI models on a subscription** rather than metered per token,
which changes what a long pipeline costs to run.

**claude** is the popular one and the original of this generation of coding agents. It is
well-understood, well-behaved in a worktree, and the harness most people already have logged in.

### Do not review your own work

**Put a different model, and ideally a different provider, on the reviewer than on the coder.**
This is the single setting that most changes what comes out of a pipeline.

A model reviewing its own output agrees with itself. It made those choices twenty minutes ago for
reasons it still finds persuasive, and the things it did not think to check are exactly the things
it will not think to check again. A different model has different blind spots, so it reads the diff
as a stranger would and asks the questions the author never asked. In practice a second opinion
from another family finds real problems: an unhandled boundary, a test that asserts the
implementation rather than the behaviour, an error path nobody ran.

The point is not that the reviewer is smarter. It is that the work reaches you in better shape,
because it has already survived somebody who was not trying to be right about it.

The **Default** team is set up this way, with `coder` on sonnet and `reviewer` on opus. Mixing
providers goes further: `coder` on claude and `reviewer` on pi with an OpenAI model, or the reverse.
Set it per role in **Team**, and per project if one repository wants something the others do not.

### Read the review

Gate the last role and read what it says before approving. The gated approval shows
`git diff base...sha`, which is everything the task would land across every role and every lap, not
just the final commit. That is the moment to catch a change that is technically correct and not what
you wanted, and it costs a minute.

Reviewing is also the cheapest place to notice that a brief was wrong. A rejection sends the work
back to the role that produced it with your note attached, which is a much shorter loop than
discovering it a week later in the base branch.

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

Four steps. The first three are things zerg checks and reports on rather than does for you. It
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

That is the whole list. No tmux, no babashka, no zsh: agents are child processes of the daemon
([§7.4](docs/ARCHITECTURE.md#74-no-tmux)), so there is no session manager to install or attach to.

### 2. Log a harness in

At least one, and zerg will not do it for you:

```sh
claude          # then /login, and answer the trust prompt in any repo you will use
pi              # then /login for the provider whose models you plan to select
```

zerg reads credential state and reports it; it never runs a login flow or touches an auth file.
Readiness will tell you exactly which of these is missing per role. See step 4.

### 3. Build and run

```sh
./build.sh          # cockpit → web/dist → embedded in the binary → ./zerg
./zerg up           # 127.0.0.1:7717
```

Or by hand, which is what `build.sh` does:

```sh
pnpm --dir web install --frozen-lockfile && pnpm --dir web build
rm -rf internal/api/dist && cp -R web/dist internal/api/dist   # go:embed cannot reach outside its package
touch internal/api/dist/.gitkeep                                # keeps a fresh clone compiling
go build -o zerg ./cmd/zerg
```

The cockpit is generated rather than committed. You do not have to build it to work on it: in a
checkout, `zerg up` starts its dev server itself and serves it on the daemon's own port, hot
reloading as you save. `./build.sh` is for producing a binary with the UI compiled in, which is
what you deploy and what a release ships.

State lives in `~/.zerg/zerg.db` (override with `--db`), which is created on first run along with
the eight built-in role templates. The directory and the database are `0700`/`0600`, since they hold every
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
from. That is the only commit zerg authors rather than an agent.

Readiness is the step worth not skipping, though you cannot really skip it: **Start refuses while
any enabled role is blocked**, and the refusal carries the report, so pressing it anyway lands you
on this screen. A team that cannot work must never reach a running board. It runs every check for
every enabled role and states a remedy for each failure:

| Check | What it catches |
|---|---|
| `binary_present` | the harness is not on PATH |
| `binary_version` | the CLI does not answer `--version` |
| `config_parses` | the CLI's own config is corrupt. Two agents racing a read-modify-write once left one holding three concatenated copies of itself |
| `credentials` (pi) | no credential for the selected provider → *run pi and use /login for that provider* |
| `workspace_trusted` (claude) | the trust prompt was never answered for that directory |
| `model_available` | the model id is not in the harness's catalog |
| `extensions_loadable` (pi) | every extension failed to load, usually a Node version mismatch |

## Reaching it from a phone

The cockpit binds to `127.0.0.1:7717` and is responsive: below 768px the nav becomes a drawer, board
lanes stack, dialogs go full screen, and the top bar names the project beside the agent count.

### What Tailscale is

A private network for your own machines. Each device runs a small daemon, gets a stable address on
a network only your devices are on, and talks to the others over an encrypted WireGuard connection
that goes directly between them wherever it can. No port forwarding, no router configuration, no
public URL, and no server in the middle holding your traffic. It works the same from home, from a
café, and from a phone on mobile data.

It is **free for personal use**: the Personal plan costs nothing, covers unlimited devices of your
own, and is [documented as non-commercial](https://tailscale.com/pricing). Installing it and
signing in with an existing identity provider is the whole setup, and this is the entire footprint
zerg needs: one machine running the daemon, one phone joined to the same tailnet.

### Why Tailscale, specifically

Because agents wait for people. A gated handoff holds until someone approves it, a role that hits a
requirement it cannot guess asks and blocks, and both happen on their schedule rather than yours,
twenty minutes after you left the desk, as often as not. A board you cannot reach from where you
are is a pipeline that is stopped until you get back to the room the machine is in. That is the
whole reason there is a phone-shaped cockpit at all.

Which leaves the question of how the phone reaches the daemon, and **zerg has no authentication**.
That is [deliberate](SECURITY.md), because the alternatives are worse than they look:

| | What it costs |
|---|---|
| **A password on the app** | An auth system I would have to build, store, rotate and get right, protecting a service whose entire job is running arbitrary code on my machine. The blast radius of getting it subtly wrong is the machine. |
| **Port-forward the router** | The daemon reachable from the internet, defended by whatever I just built. Not a thing to do with a process that spawns shells. |
| **A tunnel service** | A public URL and someone else's edge in the middle of my repositories and my agents' output. |
| **A VPN into the home LAN** | Correct, and heavier: it grants the phone the whole network to reach one port, and stops working the moment I am on a network that blocks it. |

A tailnet is the smallest thing that answers it. Devices authenticate to each other with WireGuard
keys rather than to zerg with a password; the daemon is reachable from *my* devices and from nothing
else, on hotel wifi and on LTE exactly as at home; and the trust boundary is a list of machines I
can see and revoke in an admin console, rather than a login screen I wrote.

Two details that turn out to matter more than they sound:

- **MagicDNS gives the machine a stable name**, so the cockpit is a bookmark rather than an address
  that changes with the network.
- **`tailscale cert` issues a real Let's Encrypt certificate** for that name. Not for secrecy, since
  the tailnet already encrypts everything on it, but so the phone gets no warning and so the cockpit
  runs in a secure context, which is what a browser wants before it will treat a page as an
  installable app rather than a bookmark. No self-signed certificate to trust on the phone, and
  nothing to renew by hand.

None of it is required. Without Tailscale the daemon stays on loopback and everything works from the
machine it runs on; zerg reports what is available in **Settings → Network** and says "not
available" rather than erroring. But the phone is the case this was designed around, and a tailnet
is what makes it safe to have.

### Check Tailscale first

```sh
tailscale status     # logged in, and what this machine is called
tailscale ip -4      # the address to bind to
```

zerg asks the same daemon the same questions (`tailscale status --json`), and reports the answer in
**Settings → Network**, so you can skip this and read it there. On a fresh machine the command is
faster, and four things can be wrong:

| Symptom | Meaning | Fix |
|---|---|---|
| `tailscale: command not found` | not installed | [tailscale.com/download](https://tailscale.com/download) |
| `Logged out` or a connection error | tailscaled is not running, or this machine is logged out | `tailscale up` |
| no MagicDNS name in `status` | MagicDNS is off | enable **MagicDNS** under DNS in the admin console |
| `tailscale cert <name>` fails | HTTPS certificates are off for the tailnet | enable **HTTPS Certificates** under DNS in the admin console |

Only the last one is specific to TLS; the first three are needed to reach the cockpit at all.
Everything here degrades rather than fails. A machine without Tailscale is the normal case, and
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
port, one daemon with two doors, so local work does not need the MagicDNS name, and a network setting
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
repository. Merged means the decision is recorded and the card closed, not merged means it returns
to you as pending. Swarms do not resume by themselves, which is deliberate while spawning an LLM
process costs money.

## Tests

```sh
go test ./internal/...            # coordination, routing, store, adapters
pnpm --dir web test               # the cockpit's logic: arg round-trips, stale-response guards, combobox keys
```

Neither spends a token. The coordination layer is testable without one, and is tested that way. Tests that
assert an effect check the system that was supposed to change, meaning git or the database, rather than
reading back a field the code set. [§6.1](docs/ARCHITECTURE.md#61-what-the-first-real-run-broke) is what
happens when they don't.

## Roadmap

Rough order, and none of it is promised. Anything already designed has a section in
[ARCHITECTURE.md](docs/ARCHITECTURE.md) saying what it will do and why.

**Next**

- **Move a card by hand.** Send a stuck or misrouted card to any role on the team, which is the
  missing operator control when a role dies holding work or a pipeline routes it somewhere silly.
- **Run unattended.** `zerg up` is foreground-only, so closing the terminal stops everything. A
  proper detach, plus launchd and systemd units worth copying.
- **History** ([§12.3](docs/ARCHITECTURE.md#123-what-the-history-view-answers-planned)). Spend over
  time stacked by role, cost per task ranked, wall time against active time, cache rate as a line.
  The per-turn rows already exist; the view and the rollups do not.

**Later**

- **Artifacts** ([§13](docs/ARCHITECTURE.md#13-artifacts-planned)). A screenshot, a chart, a report
  or a running dev server produced by an agent, announced on the event stream and fetched over
  plain HTTP rather than stuffed through the socket.
- **Terminal takeover** ([§10.1](docs/ARCHITECTURE.md#101-watching-an-agent-work)). Attach to a
  role's own TUI for the cases where you want to drive it yourself. Needs a pty and is not started.
- **More harnesses.** The adapter interface has two implementations, which is the minimum number
  that proves it is an interface. A third would test that claim properly.

**Being considered**

- **Authentication**, so the daemon can be exposed somewhere other than a tailnet. Deliberately
  absent today ([SECURITY.md](SECURITY.md)), and adding it badly is worse than not having it.
- **Multiple machines.** One daemon per machine today. Whether a second machine is a second cockpit
  or one cockpit over two daemons is an open question.

If you want something here, or something not here, [say so](https://github.com/kconfesor/zerg/issues).

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

Bugs, ideas and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers building it,
testing it, and the handful of things in this codebase that bite (the committed cockpit, append-only
migrations, and why `DROP TABLE` is never the answer). Security issues go
[privately](SECURITY.md).

## Licence

[Apache License 2.0](LICENSE). Copyright 2026 Kelvin Confesor.
