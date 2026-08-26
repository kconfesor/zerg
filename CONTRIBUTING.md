# Contributing

Thanks for looking. This is a small, opinionated codebase, and most of what follows is a
description of how it already works rather than rules invented for newcomers.

Start with [README.md](README.md) to run it, and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for
why it is shaped the way it is. The architecture document is not a formality — it records the
failures that produced each decision, and a change that contradicts it usually means one of us is
about to relearn something the hard way.

## Getting set up

| | Version | Why |
|---|---|---|
| **Go** | 1.27+ (`go.mod`) | the daemon. `CGO_ENABLED=0` works — the SQLite driver is pure Go |
| **Node** | 24.19.0 (`.nvmrc`) | the cockpit. Vite 8 needs `^22.18.0 \|\| >=24.12.0` |
| **pnpm** | 11 | `pnpm-lock.yaml` is the lockfile; `npm install` will not reproduce it |
| **git** | any recent | worktrees are the isolation mechanism |

```sh
git clone https://github.com/kconfesor/zerg.git && cd zerg
./build.sh          # builds the cockpit, copies it into internal/api/dist, builds ./zerg
./zerg up           # http://127.0.0.1:7717
```

`build.sh` checks your Node version itself and refuses with the one it wanted, because a build that
silently runs on the wrong version fails much further downstream.

Working on the cockpit alone is faster with Vite's dev server, which proxies the API:

```sh
./zerg up &         # the daemon on 7717
pnpm --dir web dev  # the cockpit on 5173, hot-reloading
```

## Before you open a pull request

```sh
gofmt -l .                                    # must print nothing outside web/
go vet ./... && go test ./...
pnpm --dir web lint
pnpm --dir web exec vue-tsc --noEmit          # templates, not just script blocks
pnpm --dir web test
./build.sh                                    # and commit internal/api/dist if it changed
```

CI runs all of that. It also rebuilds the cockpit and fails if `internal/api/dist` does not match
`web/` — see below.

The race detector is not in CI, because it is slow and it kept the workflow red while unrelated
things were being fixed. It has caught real bugs. Run it by hand whenever you touch anything that
supervises a process or routes work:

```sh
go test -race ./internal/agent ./internal/event ./internal/cerebrate \
              ./internal/nydus ./internal/overmind
```

## Things that will bite you

**The built cockpit is committed.** `//go:embed` needs `internal/api/dist` to exist at compile
time, so it is in git and `.gitignore` deliberately does not cover it. If you change anything under
`web/`, run `./build.sh` and commit the result, or you will ship the previous UI while every test
passes. CI checks this.

**Migrations are append-only.** Add `internal/store/schema_0NN.sql` and a line in `store.go`; never
edit one that has shipped, because a database at `user_version N` has already run the old text.

**Rebuilding a table is not a refactor.** `tasks`, `messages`, `events`, `usage_turns` and
`clarifications` are wired together with `ON DELETE CASCADE`. With foreign keys on — they are —
`DROP TABLE tasks` is an implicit delete that cascades through every transcript in the database.
When a `CHECK` constraint is in the way, add a column instead; `schema_014.sql` is the worked
example.

**Dependencies are counted.** The daemon has two non-stdlib Go dependencies (`modernc.org/sqlite`,
`coder/websocket`) and that is not an accident. The frontend adds them more readily, but each one
is argued for in `ARCHITECTURE.md §14`. A pull request that adds a library should say what was
tried without it.

**Prompts are database rows, not files.** Roles, teams, models and shared instructions are edited
in the UI and stored in SQLite. There is no config file to add, and adding one is a design change
rather than a convenience.

## Style

**Go**: standard library first, `gofmt`, errors wrapped with what was being attempted
(`fmt.Errorf("reading the card: %w", err)`). Table-driven tests where there is a table.

**Vue**: `<script setup lang="ts">`, shadcn-vue components from `web/src/components/ui`, Tailwind
utilities in the template. Types come from `web/src/lib/api.ts`, which mirrors the daemon's JSON.

**Comments explain why, not what.** The codebase is unusually heavily commented and the comments
are load-bearing: they record the failure that produced the code. `// increment the counter` is
noise; `// v-model.number returns the original string when the field is empty, which the daemon
rejects as a 400 naming neither the field nor the value` is the reason that line exists. If you fix
a bug, leave the reason behind so nobody re-introduces it.

**Commit messages are prose.** A sentence for the subject, saying what changed rather than which
files moved, then a body explaining what was wrong and why this is the fix. Look at `git log`. This
matters more than usual here, because the history is where the design rationale lives.

## Verifying UI changes

Look at it, and where a claim can be measured, measure it. Several bugs in this repo's history were
introduced by reasoning about CSS instead of reading a number back out of a browser — a spacer that
computed to zero width, a `calc()` that was silently invalid, a lane row that wrapped where it was
supposed to scroll. Screenshots in a pull request are welcome; measurements are better.

Layout that involves scroll containers is worth checking in Firefox as well as Chrome. They
genuinely disagree about what a scroll container's overflow area contains, and the board's right
gutter is an element rather than padding because of it.

## Reporting things

Bugs and ideas: [open an issue](https://github.com/kconfesor/zerg/issues). Security problems go
through [SECURITY.md](SECURITY.md) instead, privately.

Useful bug reports say what you ran, what happened, and what you expected. For anything involving
agents, the daemon's log and the card's activity transcript are usually the whole story — but read
them before pasting: a transcript contains whatever your agents read out of your repository.

## Licence

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), like the rest of the project.
