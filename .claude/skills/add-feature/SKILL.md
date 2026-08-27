---
name: add-feature
description: Add a feature to zerg end to end, across the Go daemon and the Vue cockpit. Use when implementing anything that needs both a daemon change and a UI change, when adding an API endpoint, or when a change touches the store, nydus, overmind or the board.
---

# Adding a feature to zerg

A feature here is usually four edits in the same direction: a column or a query, an endpoint, a
thing on screen, and a test that proves the effect rather than the call. This is the order that
avoids rework, and the traps that cost the most when they are hit late.

Read [docs/ARCHITECTURE.md](../../../docs/ARCHITECTURE.md) for the part you are touching before
writing anything. It records the failure behind each decision, so a change that contradicts it is
usually about to relearn something the hard way. If yours should contradict it, say why in the pull
request and update the document in the same change.

## The order

**1. Decide where the truth lives.** State is SQLite, in `internal/store`. Configuration is rows,
never files: roles, teams, models and prompts are edited in the UI, and adding a config file is a
design change rather than a convenience. If the feature needs a new column, read `.claude/commands/migration.md`
before you write any SQL, because the cascade trap in there deletes transcripts.

**2. Make the daemon do it.** The packages, by what they own:

| | |
|---|---|
| `internal/store` | schema, queries, validation. Errors that a person can fix are `invalid(...)`, which the API turns into a 400 |
| `internal/nydus` | routing work between roles: messages, routes, leases, approvals |
| `internal/overmind` | the lifecycle: start, stop, reconcile, supervise |
| `internal/cerebrate` | one role's process. Touch with care and run the race detector |
| `internal/api` | HTTP. Handlers are thin: they decode, call, and choose a status code |

**3. Put it on screen.** `web/src/lib/api.ts` mirrors the daemon's JSON and is the only place types
come from. Components use `<script setup lang="ts">`, shadcn-vue pieces from `components/ui`, and
Tailwind in the template. State that several views need lives in `App.vue` and comes down as props,
which is why the readiness report and its pending flag live there rather than in `Settings.vue`.

**4. Test the effect, not the call.** A test that asserts a function was called passes when the
system is broken. Assert against the thing that was supposed to change: the database, git, the
response. `docs/ARCHITECTURE.md` §6.1 is what happens when tests do not, and it is the record of a
card reaching Done over a branch that had never moved.

## Traps, in the order they have actually cost time

**Errors a person can fix must reach that person.** A failure with a remedy that renders as
`internal error` is worse than useless: it tells the operator nothing and hides the fix. Missing
`gh`, no remote, a wrong branch checked out, a commit git cannot resolve are all operator problems
and all return 400s that name the thing to fix. Genuine faults stay 500s. Do not widen that: a
blanket 400 told someone with no `git` on PATH that their commit did not exist.

**An error raised inside a dialog has to render inside that dialog.** The page banner sits behind
it on a phone, which is where approvals are actually read.

**The cockpit is generated, not committed.** `internal/api/dist` holds a `.gitkeep` and nothing
else. Do not commit build output, and do not run `./build.sh` while developing: `go build -o zerg
./cmd/zerg && ./zerg up` hot-reloads the UI.

**Comments explain why.** This codebase is heavily commented and the comments are load-bearing:
they record the failure that produced the line. If you fix a bug, leave the reason behind so the
next person does not reintroduce it. `// increment the counter` is noise.

**Measure UI claims.** Several bugs here came from reasoning about CSS instead of reading a number
out of a browser: a spacer that computed to zero width, a `calc()` that was silently invalid, lanes
that wrapped where they were supposed to scroll. Layout involving scroll containers is worth
checking in Firefox as well as Chrome, which disagree about what a scroll container's overflow area
contains.

**Changing a prompt is changing behaviour and cost.** Role prompts and shared instructions are
seeded in `internal/store/seed.go` and then owned by the database, so an edit reaches new
installations only. A volatile byte in a composed system prompt breaks prompt caching silently, and
the only symptom is the bill (§11.2).

## Before you say it is done

Run `.claude/commands/verify.md`, and then answer three questions in the pull request:

- What did you observe, rather than infer? For a daemon change that means the database or git; for
  the UI it means a number read back out of a browser.
- What did you leave out, and why?
- Does anything in `docs/ARCHITECTURE.md` now say something false?

Commit messages are prose: a sentence saying what changed, then a body explaining what was wrong and
why this is the fix. The history is where this project keeps its reasoning.
