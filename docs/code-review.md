# Zerg Code Re-review

**Verdict: Request changes.**

The remediation materially improved the project, and all current checks pass. However, several original issues are only partially resolved, especially authentication, swarm lifecycle, live configuration, and event accounting.

## Remaining blocking issues

### High: Stop/start/delete lifecycle is still unsafe

`internal/overmind/overmind.go:432-466` removes the swarm from `running` before teardown finishes. A new Start can therefore overlap the old Stop.

Also, `s.done` tracks `keepMoving`, not the cerebrate processes themselves. Stop can reclaim leases and return while old harnesses are still exiting, allowing old and new agents to use the same worktree.

Project deletion still bypasses Overmind entirely: `internal/api/api.go:287-292`.

**Fix:** Implement a persistent `starting/running/stopping` state, wait for all cerebrates with a `WaitGroup`, and reject or coordinate deletion of active projects.

### High: Live configuration remains inconsistent

Configuration is now refreshed before respawn, but topology and harness changes are not correctly reconciled:

- Team updates write directly to the database: `internal/api/api.go:309-324`.
- Added roles receive no cerebrate.
- Removed roles remain until they respawn.
- Renamed roles stop without a replacement.
- A harness change updates the role data, but the cerebrate retains its original adapter and config directory: `internal/cerebrate/cerebrate.go:430-481`.

This can run the old harness using the new harness's model and flags, or route work to a role with no process.

**Fix:** Either reject team/harness edits while running or make Overmind atomically add, remove, and replace affected cerebrates.

### High: Agent actions are not bound to lease ownership

`internal/agent/server.go:282-301` authenticates the caller for `done`, but discards its identity and acknowledges any lease ID.

Similarly, `send` verifies project membership but not that the role currently holds the task. A stale terminal-role token can complete a task it never claimed.

The send operation also lacks an idempotency key, so a lost response followed by retry can create duplicate handoffs and model work.

**Fix:**

- Require `(lease ID, project ID, role)` ownership for acknowledgement.
- Include the source lease in handoffs.
- Verify the task belongs to that lease.
- Guard transitions by expected task lane/state.
- Add an idempotency constraint keyed by source lease and operation.

### High: Recorder accounting can still be wrong while health reports OK

The new queue protects ordinary bursts, but:

- `failed` is never incremented.
- `written` increments even if event or usage insertion fails.
- Database errors are only logged.
- The queue is unbounded for the lifetime of the daemon.
- Task attribution happens when the writer processes an event, not when it was produced.

If task A's events remain queued until the role claims task B, those events can be attributed to B because `CurrentTaskFor` selects the newest lease.

Relevant code:

- `internal/event/recorder.go:113-234`
- `internal/store/usage.go:183-208`

**Fix:** Stamp events with immutable task/lease identity when emitted, make event and usage persistence transactional, increment accurate failure counters, and use a bounded durable spool with an explicit overflow policy.

### High: Approval crash recovery is missing

The approval race itself is fixed with `pending → integrating`, but a crash after that transition can leave the approval permanently `integrating`.

Such approvals are excluded from Attention, and there is no startup reconciliation. This is especially problematic if the merge or PR succeeded before the crash.

**Fix:** Reconcile `integrating` approvals on startup using Git ancestry or PR lookup and a stable operation key.

## Original-finding status

| Original finding | Status |
|---|---|
| Unauthenticated cockpit/RCE | **Partial** |
| Cross-project task references | **Partial** — protocol boundary scoped, DB integrity still global |
| Shared Claude parser state | **Fixed** |
| Swarm startup race and rollback | **Partial** |
| Fresh configuration on respawn | **Partial** |
| Approval decision race | **Fixed**, but crash recovery missing |
| Event durability/accounting | **Partial** |
| Team override preservation | **Partial** |
| Router/project synchronization | **Partial** |
| WebSocket error frames | **Fixed** |
| Activity/chat performance | **Partial** |
| Harness flag parsing | **Partial** |
| Agent environment filtering | **Fixed** |
| Filesystem permissions | **Partial** for existing installations |
| Task plus opening-message transaction | **Fixed** |
| SQLite read concurrency | **Not addressed** |
| Frontend lint/CI | **Fixed** |
| Frontend tests/accessibility automation | **Not addressed** |

## Vue issues still remaining

### Stale asynchronous responses

- `web/src/components/TaskDetail.vue:23-33`: opening task A then B can render A's late response under B.
- `web/src/components/layout/UsageSummary.vue:22-40`: previous-project usage can overwrite current-project usage.
- `web/src/App.vue:91-93`: workspace results are committed without checking the project.
- The `refreshFor` project-ID guard fails for A → B → A races; use a monotonically increasing request generation or `AbortController`.

### Empty argument overrides remain lossy

The backend distinguishes `nil` from an empty slice, but `ArgsOverride` uses `omitempty`. An explicit empty override disappears from JSON and becomes `null` during the next reorder.

Use a pointer or remove `omitempty` so “inherit” and “override with no arguments” remain distinct.

### Harness flag parsing is still not round-trip safe

`joinArgs()` uses JSON escaping, but `splitArgs()` does not understand escaped quotes or backslashes. Known flags are also moved ahead of custom flags, which can change order-sensitive CLI precedence.

A tag/argv editor would be safer than shell-like text.

### Pending and error states

Several mutations still allow duplicate actions:

- Start/stop
- Task creation/deletion
- Approval/rejection/answer
- Chat agent changes

`setHidden()` also has no error handling. Add per-operation pending state and disable the corresponding control.

### Accessibility and tests

- Custom model pickers still lack combobox/listbox keyboard semantics.
- Several labels are not associated with controls.
- There are still no frontend unit or E2E tests.

## Other backend improvements still needed

### Cross-project integrity remains incomplete at the database layer

Agent-provided task IDs are now scoped at the protocol boundary, but tables still independently store `project_id` and globally referenced `task_id` values.

Add `UNIQUE(project_id, id)` to tasks and composite foreign keys from messages, clarifications, events, and usage rows. Scope task reads and updates by project and require one affected row.

### Existing installations may retain permissive file modes

New prompt directories and files use `0700` and `0600`, but `MkdirAll` and `WriteFile` do not tighten existing paths. An upgraded installation can retain the previous `0755` directory or `0644` prompt/database files.

Explicitly call `Chmod` on the state directory and sensitive files, including database sidecars where applicable.

### Project creation is not fully atomic

`internal/api/api.go:254-275` commits the project before selecting its default team. If a built-in role is missing, project creation returns an error but leaves the project row and unique path behind.

Create the project and default `project_roles` in one store transaction.

## Performance notes

- `SetMaxOpenConns(1)` still serializes every SQLite read and write. This negates WAL reader concurrency and increases recorder backlog risk.
- Activity batching is a good improvement, but all 2,000 rows remain mounted and Markdown is regenerated during rendering.
- Chat is now capped at 500 rows, but still updates reactively and scrolls per event.
- Bundle size is approximately **421 kB JS / 132 kB gzip**, up slightly from the previous review but still reasonable.

## Documentation and CI

- CI now enforces formatting, vet, Go tests, frontend linting, Vue type-checking, builds, and embedded cockpit freshness.
- The race detector passes locally but is disabled in CI. Restore it or run it as a scheduled job.
- `ARCHITECTURE.md` still describes `zerg up --detach` and harness session resume even though those paths are not implemented.
- Settings report retention as immediately applied, but the retention duration is captured at daemon startup.
- Restart detection considers only the address, not TLS mode, certificate paths, or local-listener settings.

## Strong improvements

- Claude parser state is now session-local.
- Concurrent Start requests are reserved and failed starts clean up sessions and tokens.
- Task creation and its opening message are transactional.
- Approval decisions use a guarded claim.
- Agent environments use a credential-conscious allowlist.
- WebSocket errors are surfaced and retried.
- Activity updates are batched per animation frame.
- Chat history is bounded.
- Team overrides are substantially better preserved.
- CI catches stale embedded frontend assets.

## Verification

All passed on the clean tree:

```text
go test -count=1 ./...
go vet ./...
go test -count=1 -race ./internal/agent ./internal/event \
  ./internal/cerebrate ./internal/nydus ./internal/overmind

pnpm --dir web lint
pnpm --dir web build
git diff --check
```

The embedded cockpit matches `web/dist`.
