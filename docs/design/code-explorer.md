# Code explorer

Closes issue [#43]. A standalone way to walk any branch, tag or commit of a project's repository —
not just the ref an approval happens to be sitting on — read any file in it with real syntax colour,
and ask the project's own agent to explain a file or a selection without leaving the page. The
existing diff viewer is untouched: this is a second surface, reachable independent of any task, for
"what does this file actually say" rather than "what changed."

Every read comes from git's object database against the project's root repository — the same source
every diff in this codebase already reads from — never from a worktree checkout. Nothing here needs
a schema migration. The whole surface is three new git-plumbing functions, three read-only routes
and one write route, one new frontend nav destination, and — new to this codebase, and chosen on
purpose — a syntax-highlighting dependency.

[#43]: https://github.com/kconfesor/zerg/issues/43

## What already exists to build on

Facts checked against the tree at proposal time.

| | |
|---|---|
| `Hatchery.Resolve` | `git rev-parse <ref>` → sha. Reusable as-is for a manually typed ref/sha in the picker |
| `Hatchery.resolves` | `git rev-parse --verify --quiet <ref>^{commit}`, answers "does this ref exist" with an exit code rather than parsed stderr prose. The right pre-check for a client-supplied ref |
| `Hatchery.Diff` / `ChangedFiles` / `RangeFiles` | Whole-commit and range diffs, with size (`maxFile = 256*1024`, duplicated in `features.go` and `settings.go`) and binary (`git diff --numstat` reporting `-\t-`) guards — all of it shaped for *a diff*, not a bare file at a ref |
| `Hatchery.LoadFile` / `load` | The closest existing "read one file" primitive. Wants a `base` for its diff half, and reads the file with `git show sha:path` into an in-memory buffer via the shared `git()` helper (unbounded stdout capture) and only *then* compares `len(body)` against the byte cap (`worktree.go:583-589`) — buffer-then-check, not stat-then-read. See decision 3 |
| `internal/api/settings.go`: `askAboutTheChange` / `answerInThread` / `askPrompt` | 202 + background `AskAndWait`, the answer landed as a comment on a `ReviewThread`. Embeds the hunk text directly in the prompt — "asked about what is on the reader's screen." The template for a selection-scoped "ask AI" |
| `internal/api/settings.go`: `requestGuide` / `buildGuide` / `guidePrompt` | 202 + background `AskAndWait`, but *names* a ref/commit and tells the agent to read it itself (`git show`/`git diff` in this repository) rather than pasting content. Pre-checks `chatMgr.Busy` and 409s. The template for a whole-file "explain this" |
| `internal/chat/chat.go`: `AskAndWait` / `reviewChat(projectID)` | One question, one answer, no chat tab, serialized per project by `m.asking` and `beginTurn`/`endTurn`. Already the transport code-explorer needs — no new agent, no new worktree |
| `internal/api/artifacts.go` | Content-addressed ETag + `immutable` caching; inline-vs-download is an **allowlist**, not a blocklist; `X-Content-Type-Options: nosniff` + a restrictive CSP. The template for serving arbitrary repo bytes to a browser without ever executing them |
| `internal/api/guard.go` | Wraps the entire mux already. A new route gets the CSRF/DNS-rebinding check and the 1 MB body cap for free, no extra code |
| `internal/api/icons.go`: `resolveInside` | Guards a *filesystem* path against traversal, because that code reads real files. Cited only as contrast — see decision 1 |
| `web/src/router.ts`'s flat `VIEWS` array + `App.vue`'s per-view `v-else-if` chain | Adding a top-level nav destination is: push a name onto `VIEWS`, add one lazy-loaded component, add one template arm. Project-scoped routing is generic already |
| `Board.vue` / `DiffView.vue`, `ee1dc4c`'s commit message | The established mobile checkpoint: `sm:` (640px) is the breakpoint every other view is built and measured against, at 390px specifically |

Missing, confirmed by grepping `internal/` for `ls-tree`, `for-each-ref`, `branch --list`, `tag`:

- No directory listing at a ref, no ref/branch listing, no ref-agnostic single-file read, no commit
  log, no blame. Zero hits.
- No file-tree / repo-browser component anywhere in `web/src`.
- `web/package.json` carries 15 runtime dependencies today; none is a highlighter or code editor.
- Every existing caller of a `Hatchery` git-wrapping function passes a ref/sha the **daemon itself**
  derived — `project.BaseBranch`, `approval.Commit`, a resolved `task` — never one taken directly off
  an HTTP query string. Code explorer's `ref` and `path` are the first client-supplied strings that
  will reach a `git` argv. See decision 2.
- `store.ReviewThread` cannot exist without a task: `OpenReviewThread` calls
  `db.GetTask(ctx, t.TaskID)` and fails the whole open if it doesn't resolve (`internal/store/review.go:79`).
  The existing "ask AI" persistence model is inseparable from a task, and code explorer is explicitly
  not. See decision 9.

## Decisions

### 1. Reads are git-object plumbing against `h.repoPath`, never a worktree — and get a new, lighter type

New `Hatchery` surface, not an extension of `ChangedFile`:

```go
type Ref struct {
    Name string // short name: "main", "v1.2.0"
    Kind string // "branch" | "tag"
    SHA  string
}
func (h *Hatchery) Refs(ctx context.Context) ([]Ref, error)

type TreeEntry struct {
    Name string
    Path string
    Kind string // "blob" | "tree" | "commit" (submodule)
    Size int64  // blobs only
}
func (h *Hatchery) Tree(ctx context.Context, ref, dir string) ([]TreeEntry, error)

type Blob struct {
    Path     string
    Content  string
    Size     int64
    Binary   bool
    TooLarge bool
}
func (h *Hatchery) Blob(ctx context.Context, ref, path string, maxBytes int) (*Blob, error)
```

**Why.** `.worktrees/preview` is one shared worktree, force-checked-out detached on every "Run this
change"; `.worktrees/chat-<id>` is per-conversation but still one mutable checkout. Neither is ever
"the" copy of a specific ref someone is browsing. Every worktree in a project shares one object
database, so `git ls-tree <ref>` and `git show <ref>:<path>` answer correctly from `h.repoPath`
regardless of what happens to be checked out anywhere.

**What it actually costs.**

- Three new functions, not one generalized `LoadFile`. `ChangedFile.Diff`, `.Added/.Removed` and
  `.Deferred` describe a diff against a base and an eager-batch cutoff — none of that applies to a
  bare file at a ref.
- Path traversal is not a concern for `Tree`/`Blob` the way it is for `icons.go`'s `resolveInside`.
  `ref:path` syntax is resolved by git against its own tree objects — `git show main:../../../etc/passwd`
  fails with "does not exist in 'main'," it never walks the real filesystem. Cited for contrast, not
  imitated.
- `maxFile = 256*1024` is currently a local `const` in both `features.go` and `settings.go`. Worth
  hoisting to an exported `hatchery.MaxFileBytes` when this lands.

### 2. `ref` and `path` are user input reaching a git argv for the first time — guarded explicitly

**Why.** Every other place in this codebase that hands a ref to `git` gets it from a stored row — a
project's path, an approval's commit, a task's branch. Code explorer's `ref` and `path` come straight
off an HTTP query string. A ref beginning with `-` is not hypothetical here.

**What it actually costs.**

- `Tree` and `Blob` call the existing `resolves(ctx, ref)` before doing anything else, the same
  pre-check `changed()` already uses for `sha`/`base`. Anything that isn't a real revision is
  rejected as `ErrNoSuchRevision` before it reaches `ls-tree` or `show`.
- Pass the resolved ref/path after git's own end-of-options marker rather than trusting the string
  never starts with a dash.

### 3. A file read stats before it reads — not `LoadFile`'s pattern

**Why.** `load()`'s `git show sha:path` reads the whole blob into memory before comparing `len(body)`
to `maxBytes`. Safe today because it only ever runs on `isDoc()` files (`.md`, `.txt`) within the
first `eagerFiles = 30` of one commit's changes — a small, implicitly-bounded population. Code
explorer has no such bound: any file, any ref, directly addressable by URL.

**What it actually costs.** `Blob` does two git invocations: `git cat-file -s <ref>:<path>` for the
size (git answers this without reading the object's content), and only if that size is within
`maxBytes`, `git show <ref>:<path>` for the bytes. Binary detection: sniff the first few KB for a NUL
byte, the same heuristic git's own diff machinery uses. Two processes per open file is fine — this
is one file, opened because a person clicked it, not a thirty-file eager batch.

### 4. Browsing pins to the resolved commit sha, not the moving ref name, once navigation starts

**Why.** A ref picker showing "main" is showing a name that can move mid-session. Without pinning,
clicking into a directory and then opening a file inside it are two separate resolutions of "main,"
and they can disagree. This is the first surface in the codebase that lets a person browse a name
that moves rather than a stored, pinned commit.

**What it actually costs.** `Tree` and `Blob` responses both carry the sha they actually resolved.
The frontend keeps that resolved sha in navigation state and the URL from the first response
onward, re-resolving the ref name only on an explicit re-pick. A shared link shows "main @ a1b2c3d"
rather than a bare branch name — the useful side effect of the same mechanism.

### 5. New routes: project-scoped, lazy per directory

```
GET  /api/projects/{id}/refs                      → Ref[]
GET  /api/projects/{id}/tree?ref=<ref>&path=<dir>  → { resolvedSha, entries: TreeEntry[] }
GET  /api/projects/{id}/file?ref=<ref>&path=<file> → Blob (+ resolvedSha)
```

**Why lazy per directory.** `git ls-tree <ref> <dir>` (no `-r`) is non-recursive and returns names,
types and sizes with no content — there's no need for an "eager 30" here, because listing a
directory never fetches file content. Content is fetched exactly once, when a specific file is
opened.

**What it actually costs.** These sit alongside the existing flat project-scoped routes
(`/attention`, `/usage`, etc.) rather than a new `/code/` prefix, matching how `features`/`history`/
`run` are already flat. `guard.go` and the 1 MB body cap apply automatically.

### 6. Shiki, loaded per file extension, skipped past a size cutoff

**Why Shiki over highlight.js/Prism.** Both alternatives are regex-grammar engines with a documented
history of catastrophic-backtracking bugs — a real concern for a surface whose job is rendering
arbitrary, untrusted repo content. Shiki compiles the same TextMate grammars VS Code and GitHub use.
Modern Shiki ships a pure-JS regex engine (`shiki/engine/javascript`) as an alternative to the WASM
oniguruma engine, plus fine-grained entry points so a grammar and theme can be imported per file.

**How it's loaded.** The JS engine (not WASM), a small extension→language map, `import()`-ed the
first time a file of that language is opened. Dual light/dark theme output, switched by the
cockpit's existing `.dark` class.

**What it actually costs.** Past a size cutoff (well under the 256 KB fetch cap), the file renders
as plain monospace text with a note, and highlighting is skipped entirely — the guard against a
pathological grammar on a pathological input. Frontend-only addition; no new backend surface.

### 7. A new top-level nav destination, checked at phone width from phase 1

**Why.** `VIEWS` is a flat array and `App.vue` a flat `v-else-if` chain — adding "code" is additive.
But the mobile discipline this codebase holds everywhere (`Board.vue`'s lanes stacking under `sm:`,
`ee1dc4c`'s 390px measurements) is a property of how a view is built, not applied afterward.

**What it actually costs.** Below `sm:`, one pane is visible at a time — ref picker, then tree, then
file — with a back affordance. Above it, three columns. This has to be true from the first phase's
UI; retrofitting it onto an already-shipped desktop-only layout is strictly more work.

### 8. Explain reuses `chat.Manager.AskAndWait` against the existing `reviewChat(projectID)` conversation

**Why.** A code-explorer-specific agent/worktree/conversation would duplicate exactly what
`reviewChat(projectID)` already does. Both `askAboutTheChange` and `requestGuide` already prove this
pattern works for "ask AI about code I'm looking at."

**What it actually costs.** Explain contention is real and shared: browsing code and asking
"explain this file" while a teammate is mid-review-question **in the same project** collide via
`AskAndWait`'s `m.asking` mutex. Not new risk — the existing `ErrBusy`/409 behavior `requestGuide`
already has a UX for, extended to a second caller. Must surface as "busy, try again," not silently
retried or dropped.

### 9. Explain answers are ephemeral for v1, not a `ReviewThread`

**Why not the existing thread model.** `OpenReviewThread` hard-requires a task. Reusing it for a
file browsed at an arbitrary ref with no task in sight means either inventing a fake task per
browsing session (pollutes the task list, board, rework counting) or loosening a hard FK check that
exists precisely so a remark's gate always points at something real. Both are bigger and riskier
than the value of persisting a browse-time answer.

**What it actually costs.** An in-memory, per-daemon job map (`sync.Mutex` + map, same shape as
`chat.Manager`'s own maps), swept on a TTL. `POST /api/projects/{id}/explain` returns 202 with a
job id; `GET /api/projects/{id}/explain/{jobId}` is polled for `{status, answer}` — the same
202-then-poll idiom as `requestGuide`, minus the database row. An answer does not survive a daemon
restart, is not shareable by URL, and does not show up elsewhere in the cockpit — explicitly
acceptable for v1.

### 10. Two prompts for two triggers, not one generalized prompt

**Why.** `askPrompt` and `guidePrompt` already model two shapes worth keeping distinct:
`askPrompt` embeds text directly so the agent answers about exactly what's on screen;
`guidePrompt` names a ref/commit and lets the agent read it with its own tools. "Explain this
file" is guide-shaped (name `ref:path`, let the agent `git show` it in its own worktree); "explain
this selection" is ask-shaped (embed the selected lines directly).

**What it actually costs.** Two prompt-building functions instead of one, mirroring code that
already exists. Reuses `askTimeout` (3 minutes) as-is.

### 11. No `TaskID` on explain's usage turns, for v1

**Why.** `usage_turns.task_id` is nullable and chat's own turns already leave it unset — an
existing gap, not a new one. Code explorer has no task to stamp even if it wanted to.

**What it actually costs.** Explain spend is invisible in any per-task or per-file cost view for
v1 — the same blind spot chat already has. Fixing it is a bigger change than this feature justifies
on its own.

## Phases

1. **Browse, plain text.** `Refs`/`Tree`/`Blob` on `Hatchery`, the three read routes, and a new
   "Code" nav destination: ref picker, lazy directory tree, a plain monospace file pane. Sha-pinning
   (decision 4), the stat-before-read guard (decision 3) and the end-of-options guard (decision 2)
   are the correct first implementation, not a follow-up patch. Checked at 390px from the start.
   Demoable: pick a branch or paste a sha, walk into a directory, open a file, read it — desktop and
   phone. No highlighting, no AI. This alone is most of issue #43's "IDE-like file exploration" ask.
2. **Syntax highlighting.** Shiki per decision 6: per-extension lazy grammar, dual theme, size
   cutoff falling back to plain text. Purely additive to phase 1's frontend.
3. **Explain.** The `explain`/`explain/{jobId}` routes, `AskAndWait` against `reviewChat(projectID)`,
   the two prompt shapes from decision 10, and two UI triggers: a persistent "Explain this file"
   toolbar action, and a selection popover that appears only when text is highlighted. Demoable:
   select a few lines or click "explain file," get an answer, including seeing "busy, try again"
   when it collides with an in-flight review question elsewhere in the project.

Order argument: 2 before 3 because it's the cheaper, purely-additive, zero-new-runtime-surface
change, and it makes 3's UI read better once it exists — not a hard dependency; either could ship
first without breaking the other.

## Failure modes to design against

- **A huge blob read in full before its size is checked**, by copying `LoadFile`'s pattern instead
  of decision 3's stat-then-read.
- **A ref or path string reaching git's argv as if it were an option.**
- **A tree shown for a ref that has since moved or been deleted** between clicks — bounded by
  sha-pinning, but the first resolution is still a race with whoever pushes next; accepted.
- **A pathological file hanging the highlighter or the tab** — guarded by skipping highlighting past
  a size cutoff.
- **Repo content rendered as live DOM instead of inert text.** An HTML or SVG file in the repo is
  the same threat `artifacts.go`'s allowlist exists to contain; never `v-html` or iframe repo bytes
  without a sandbox.
- **An explain request queuing invisibly** behind an unrelated review question, read as "the button
  did nothing" rather than "busy."
- **A client polling a job id that no longer exists** after a daemon restart — the poll endpoint
  says "not found" plainly.

## Still open

- Whether explain Q&A earns a persisted, task-independent thread type once real usage shows people
  want history or a shareable answer link. Not built now.
- Whether the ref picker filters zerg's own housekeeping branches (`zerg-<role>`, `zerg/<task>`,
  `zerg-feature/<id>`) out by default, or shows everything `for-each-ref` returns.
- A "download raw file" affordance — would follow `artifacts.go`'s allowlist split exactly if added.
- Directory-listing pagination for a pathologically large single directory — not built until measured.
- Binary file preview (images, fonts). v1 reports "binary, N bytes" and stops there.
- `TaskID`/cost rollup for explain turns — deferred, the same gap chat's own turns already have.
- Symbol/method-level explain — explicitly out of scope per the agreed v1 scope.
