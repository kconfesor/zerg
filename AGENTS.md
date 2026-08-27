# Working on zerg

Guidance for any coding agent working in this repository, and for people, who are held to the same
rules. This file is the canonical copy: the Claude Code commands under `.claude/` and the human
guide in [CONTRIBUTING.md](CONTRIBUTING.md) both defer to it.

zerg is a Go daemon that supervises coding agents in isolated git worktrees, routes work between
them, and serves a Vue 3 cockpit. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) says how and, more
usefully, why: it records the failure behind each decision. Read the section covering what you are
about to touch. If your change contradicts it, say so and update it in the same change.

## The loops

```sh
go build -o zerg ./cmd/zerg && ./zerg up   # working on it: ~2s, cockpit hot-reloads as you save
./build.sh                                 # producing a binary to run: ~11s, cockpit compiled in
```

Do not run `./build.sh` while developing. It costs eleven seconds and produces assets thrown away on
the next edit. `zerg up` starts the cockpit's dev server itself when nothing is compiled in.

## Before you call anything done

```sh
gofmt -l . | grep -v '^web/'          # must print nothing
go vet ./... && go test ./...
pnpm --dir web lint
pnpm --dir web exec vue-tsc --noEmit  # templates, not just script blocks
pnpm --dir web test
```

Two ways that comes back green and means nothing:

- **A skip is not a pass.** Three tests in `internal/api` skip when the cockpit is not built. CI
  runs them for real in a separate job. Do not report a skip as coverage.
- **An intermittent failure is not automatically a flake.** `TestFatalErrorStopsSupervision` looked
  like one and was a real race that let the supervisor respawn into a fatal error. Try `-count 20`
  and `-cpu 1` before deciding; that is what made it deterministic.

## Where things live

| | |
|---|---|
| `internal/store` | SQLite: schema, queries, validation. Errors a person can fix are `invalid(...)`, which the API renders as 400 |
| `internal/nydus` | routing work between roles: messages, routes, leases, approvals |
| `internal/overmind` | lifecycle: start, stop, reconcile, supervise |
| `internal/cerebrate` | one role's process. Touch carefully and run the race detector |
| `internal/api` | HTTP. Handlers decode, call, and choose a status code |
| `internal/devui` | the cockpit's dev server, when running from a checkout |
| `web/src` | the cockpit. `lib/api.ts` mirrors the daemon's JSON and is where types come from |

## Rules that have been paid for

**Configuration is rows, not files.** Roles, teams, models and prompts live in SQLite and are edited
in the UI. Adding a config file is a design change, not a convenience.

**Migrations are append-only.** Add `internal/store/schema_0NN.sql` plus its line in `store.go`.
Never edit one that has shipped: a database at `user_version N` has already run the old text.

**Rebuilding a table is not a refactor.** `tasks`, `messages`, `events`, `usage_turns` and
`clarifications` are wired with `ON DELETE CASCADE`, and foreign keys are on, so `DROP TABLE tasks`
is an implicit delete that cascades through every transcript in the database. When a `CHECK`
constraint is in the way, add a column instead. `schema_014.sql` is the worked example. Verify a
migration against a copy of a real database, not only a fresh one, because a fresh one cannot show
you what was destroyed.

**An error a person can fix must reach that person.** Missing `gh`, no remote, the wrong branch
checked out, a commit git cannot resolve: all operator problems, all 400s naming the thing to fix.
Genuine faults stay 500s. Do not widen this into a blanket 400 either, which once told someone with
no `git` on PATH that their commit did not exist.

**An error raised inside a dialog renders inside that dialog.** The page banner is behind it on a
phone, which is where approvals actually get read.

**The cockpit is generated, not committed.** `internal/api/dist` holds a `.gitkeep` and nothing
else. Never commit build output.

**Dependencies are counted.** Two non-stdlib Go dependencies, deliberately. A change that adds a
library should say what was tried without it.

**Prompts are behaviour and cost.** Role prompts and shared instructions are seeded in
`internal/store/seed.go` and owned by the database afterwards, so an edit reaches new installations
only. A volatile byte in a composed system prompt breaks prompt caching silently, and the bill is
the only symptom.

**Test the effect, not the call.** Assert against the thing that was supposed to change: the
database, git, the response. A test that checks a function was called passes while the system is
broken, which is what ARCHITECTURE §6.1 is the record of.

**Measure UI claims.** Several bugs here came from reasoning about CSS instead of reading a number
out of a browser: a spacer that computed to zero width, a `calc()` silently invalid, lanes that
wrapped where they should have scrolled. Anything involving scroll containers is worth checking in
Firefox as well as Chrome, which disagree about what a scroll container's overflow area contains.

**Comments explain why.** The comments here are load-bearing: they record the failure that produced
the line. If you fix a bug, leave the reason behind. `// increment the counter` is noise.

## Reporting what you did

Say what you observed, not what you expect to be true. For a daemon change that means the database
or git; for the cockpit it means a number read back out of a browser. Say what you left out and why.
If something in `docs/ARCHITECTURE.md` is now false, fix it here rather than leaving it.

Commit messages are prose: a sentence saying what changed, then a body explaining what was wrong and
why this is the fix. The history is where this project keeps its reasoning.
