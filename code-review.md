# Zerg Code Review

**Verdict: Request changes before exposing the cockpit beyond a trusted development machine.**

The project has a strong architecture and unusually good backend tests, but several concurrency, authorization, and state-consistency issues could cause cross-project corruption, duplicate work, inaccurate accounting, or remote code execution.

## Critical

### 1. Unauthenticated cockpit can become remote code execution

- All API routes are unauthenticated: `internal/api/api.go:76-139`
- Request bodies accept any content type and have no size limit: `internal/api/api.go:590-598`
- Claude defaults to `bypassPermissions`: `internal/adapter/claudeharness/claude.go:137-143`
- HTTP only configures `ReadHeaderTimeout`: `cmd/zerg/main.go:222-228`

Anyone who can reach the port can edit prompts, create tasks, start agents, and read repository data. The default loopback listener is also potentially vulnerable to DNS rebinding and cross-site requests because Host, CSRF, and Fetch Metadata are not validated.

**Fix:**

- Require a random bearer token or authenticated session for API and WebSocket access.
- Enforce an allowed Host list and validate Origin/`Sec-Fetch-Site`.
- Require `application/json` for mutations and cap request bodies with `http.MaxBytesReader`.
- Refuse non-loopback binding unless authentication is configured.
- Add `ReadTimeout`, `IdleTimeout`, and appropriate endpoint-specific limits.

## High-priority backend issues

### 2. Agents can reference tasks belonging to another project

`internal/agent/server.go:315-356` checks task IDs using global `GetTask`; only the name fallback is project-scoped. The task ID then passes through routing and task updates without verifying its project.

A project-A agent could create messages referencing or complete a project-B task.

**Fix:** Resolve every task by `(project_id, task_id)`. Add database-level composite foreign-key integrity between messages and tasks. Apply the same rule to clarification task IDs.

### 3. Claude parser state is shared across every agent

The registry stores one adapter instance:

- `cmd/zerg/main.go:132-134`
- `internal/adapter/registry.go:12-38`

Claude's adapter stores current model and billing as mutable atomics:

- `internal/adapter/claudeharness/claude.go:32-64`
- `internal/adapter/claudeharness/claude.go:317-374`

Concurrent Claude roles can overwrite each other's model/billing values. This avoids a Go data race but produces incorrect usage attribution.

**Fix:** Register adapter factories and instantiate a parser per subprocess/session. Keep only immutable capabilities/model discovery on shared objects.

### 4. Swarm startup is race-prone and not failure-atomic

`internal/overmind/overmind.go:267-378` checks whether a project is running, unlocks, starts a session and processes, then finally registers the swarm.

Consequences:

- Two concurrent Start requests can both launch a swarm.
- A failure after the first role starts can leave orphaned processes, tokens, and an open session.
- Deleting a project does not coordinate with a running swarm: `internal/api/api.go:265-272`.

**Fix:** Introduce `starting/running/stopping` lifecycle states reserved under the mutex. Roll back tokens, sessions, contexts, and started cerebrates on any startup failure. Reject or coordinate project deletion while active.

### 5. Live configuration contradicts the architecture

The architecture says prompts are read fresh on every spawn, but `Overmind.Start` reads the team, settings, and prompts once and stores them in `cerebrate.Config`:

- `internal/overmind/overmind.go:287-364`
- `internal/cerebrate/cerebrate.go:605-624`

A crash respawn therefore reuses stale configuration. Team changes are more dangerous: routing sees the new database team while the running cerebrates remain the old set.

The UI acknowledges the mismatch at `web/src/App.vue:379-383`.

**Fix:** Either reject runtime-affecting edits while running or reconcile/restart affected roles through Overmind. Resolve configuration immediately before each spawn. Update `ARCHITECTURE.md` until this is implemented.

### 6. Approval and rejection can race after integration

`internal/nydus/nydus.go:827-900`:

1. Reads a pending approval.
2. Releases the transaction.
3. Performs Git/PR integration.
4. Updates the approval without `WHERE state='pending'`.

Concurrent approve/reject calls can overwrite each other after an irreversible merge or PR operation.

**Fix:** Atomically transition `pending → integrating` using compare-and-swap, perform integration, then finalize with another guarded transition.

### 7. Durable event and cost records can silently disappear

The event bus intentionally drops messages when subscriber buffers fill:

- `internal/event/bus.go:77-114`
- Recorder buffer: `internal/event/recorder.go:20-56`

The recorder performs task attribution and one or two database writes per event. A burst or SQLite delay can drop transcripts and billing rows. Drop counts disappear when the subscription ends and health always reports OK.

**Fix:** Give persistence a dedicated durable queue or backpressure path, batch inserts, cache task attribution briefly, and expose recorder lag/drop/write-failure metrics. Usage accounting should not share best-effort browser telemetry semantics.

## Vue findings

### 8. Team edits can erase argument overrides

`web/src/components/TeamEditor.vue:64-73` reconstructs team entries using only `modelOverride`. It omits `argsOverride` and derives the model override from the generic `overridden` boolean.

Reordering or toggling a role can silently clear existing argument overrides.

**Fix:** Return explicit `modelOverride` and `argsOverride` values from the backend and round-trip both unchanged.

### 9. URL project and displayed project can diverge

`web/src/App.vue:45-58,228-280,422-438` derives the view from Vue Router but only changes `current` through `open()`. Browser back/forward between two project URLs can change the URL without changing the loaded project.

Concurrent polling and project switches can also let an old request overwrite the newly selected project's data.

**Fix:** Watch `route.params.projectId` as the canonical project selection. Cancel stale requests with `AbortController` or use a load-generation token before committing results.

### 10. WebSocket error frames are ignored

The server emits an in-band `error` frame when replay fails, but `web/src/lib/api.ts:379-396` handles only `activity` and `caught-up`.

The activity screen can remain stuck on “connecting” indefinitely.

**Fix:** Use a discriminated frame union, handle `error`, expose it to consumers, and close/retry or provide an explicit retry action.

### 11. Frontend performance will degrade on long streams

`web/src/components/Activity.vue:38-65` mutates a reactive array and schedules scrolling for every event while retaining up to 2,000 rendered rows. Markdown is regenerated during rendering. Chat has no cap.

**Fix:** Batch events once per animation frame, precompute rendered Markdown, virtualize the activity list, and cap/page chat history.

### 12. Harness flag parsing can corrupt arguments

`web/src/components/Settings.vue:142-169` classifies arguments using flattened known tokens and splits custom input on whitespace. A custom argument following a known flag may disappear, and quoted argument values cannot be represented correctly.

**Fix:** Match complete known sequences by index and retain all unmatched argv entries. Prefer an array/tag editor over shell-like free text.

## Other improvements

- `AgentEnv` inherits the daemon's complete environment, potentially exposing cloud credentials and tokens to agents: `internal/adapter/adapter.go:109-128`. Use an allowlist.
- Database/state directories and prompt files are created as `0755`/`0644`: `internal/store/store.go:68-79`, `internal/cerebrate/cerebrate.go:611-625`. Prefer `0700`/`0600`.
- `NewTask` creates the task separately from its opening message: `internal/nydus/nydus.go:92-115`. Make this one transaction.
- `SetMaxOpenConns(1)` serializes reads and writes, negating WAL reader concurrency: `internal/store/store.go:73-86`. Consider a single writer plus bounded read pool after benchmarking.
- There are no frontend tests, linting, accessibility checks, or CI configuration.
- Some architecture sections describe unimplemented features—`daily_rollup`, history UI, price tables, task pinning—as current behavior.

## Strong patterns

- Good transactional lease and route handling.
- Commit references are resolved to absolute SHAs in the sender worktree.
- Tests frequently verify actual Git/filesystem effects rather than derived flags.
- Parameterized SQL and allowlisted dynamic grouping.
- Strong process-group cancellation and bounded WebSocket writes.
- Vue strict type-checking passes.
- WebSocket cursor replay/backoff is well designed.
- The custom Markdown renderer escapes input before generating HTML; no current XSS bypass was identified.

## Verification performed

All passed:

```text
go test ./...
go vet ./...
go test -race ./internal/agent ./internal/event ./internal/cerebrate ./internal/nydus ./internal/overmind
pnpm --dir web build
git diff --check
```

Frontend bundle: approximately **405 kB JS / 128 kB gzip**.
