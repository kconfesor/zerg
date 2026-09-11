# Code explorer

A standalone way to walk any branch, tag or commit of a project's repository — not just the ref an
approval happens to be sitting on — read any file in it with real syntax colour, and ask the
project's own agent to explain a file, a folder or a selection without leaving the page. The
existing diff viewer is untouched: this is a second surface, reachable independent of any task, for
"what does this file actually say" rather than "what changed."

**What this settles, against what issue [#43] asked for.** #43 wants a full code explorer *and*
diff viewer, "click on any line/method/file/folder," and says the current diff viewer "could be kept
or replaced by the full featured one." This doc covers the explorer half in full for file/folder/
selection granularity; it explicitly leaves the diff viewer as-is (a richer diff viewer, if wanted,
is a separate doc) and explicitly excludes method/symbol-level explain (no source-parsing
dependency in v1 — selecting a function's lines already covers "explain this function"). Both
exclusions were agreed before this doc was written, not discovered while writing it.

Every read comes from git's object database against the project's root repository — the same source
every diff in this codebase already reads from — never from a worktree checkout. Nothing here needs
a schema migration. The whole surface is three new git-plumbing functions, three read-only routes
and one write route, one new frontend nav destination, a required fix to `chat.Manager`'s existing
serialization (decision 8), and — new to this codebase, and chosen on purpose — a syntax-highlighting
dependency.

[#43]: https://github.com/kconfesor/zerg/issues/43

## What already exists to build on

Facts checked against the tree at proposal time.

| | |
|---|---|
| `Hatchery.Resolve` | `git rev-parse <ref>` with no `--verify` — **not safe for a client-supplied ref as-is**: `git rev-parse docs` in this repo exits 0 and echoes `docs`, not a sha. See decision 2 |
| `Hatchery.resolves` | `git rev-parse --verify --quiet <ref>^{commit}`, the right verify form, but returns only a `bool` and discards the sha it proved exists. See decision 2 |
| `Hatchery.Diff` / `ChangedFiles` / `RangeFiles` | Whole-commit and range diffs, with size (`maxFile = 256*1024`, duplicated in `features.go` and `settings.go`) and binary (`git diff --numstat` reporting `-\t-`) guards — all of it shaped for *a diff*, not a bare file at a ref |
| `Hatchery.LoadFile` / `load` | The closest existing "read one file" primitive. Wants a `base` for its diff half, and reads the file with `git show sha:path` into an in-memory buffer via the shared `git()` helper (unbounded stdout capture) and only *then* compares `len(body)` against the byte cap (`worktree.go:583-589`) — buffer-then-check, not stat-then-read. See decision 3 |
| `internal/api/settings.go`: `askAboutTheChange` / `answerInThread` / `askPrompt` | 202 + background `AskAndWait`, the answer landed as a comment on a `ReviewThread`. Embeds the hunk text directly in the prompt — "asked about what is on the reader's screen." The template for a selection-scoped "ask AI" |
| `internal/api/settings.go`: `requestGuide` / `buildGuide` / `guidePrompt` | 202 + background `AskAndWait`, but *names* a ref/commit and tells the agent to read it itself (`git show`/`git diff` in this repository) rather than pasting content. The template for a whole-file "explain this" — its `chatMgr.Busy` precheck is dead code (wrong map key, see decision 8), not a working example to copy |
| `internal/chat/chat.go`: `AskAndWait` / `reviewChat(projectID)` | One question, one answer, no chat tab. Already the transport code-explorer needs — no new agent, no new worktree — but its serialization is a single daemon-wide `sync.Mutex`, not per-project or per-conversation as it first looks. See decision 8 |
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
database, so `git ls-tree` and `git show <ref>:<path>` answer correctly from `h.repoPath` regardless
of what happens to be checked out anywhere.

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
- **`Blob` cannot reuse the shared `git()` helper.** Checked directly: `git()` (`worktree.go:310`)
  returns `strings.TrimSpace(stdout.String())` — every other caller wants that, because every other
  caller is reading a sha, a subject line, or a diff meant for a terminal. A blob is exact bytes: a
  file with leading blank lines, deliberate trailing whitespace, or no trailing newline would come
  back silently altered, and every selected line number downstream would be wrong by however much
  got trimmed. `Blob` needs its own byte-preserving invocation (`cmd.Output()` into a `[]byte`, no
  `TrimSpace`), not the shared helper.

### 2. `ref` and `path` are user input reaching a git argv for the first time — resolved once, correctly, not checked-then-trusted

**Why.** Every other place in this codebase that hands a ref to `git` gets it from a stored row — a
project's path, an approval's commit, a task's branch. Code explorer's `ref` and `path` come straight
off an HTTP query string. Two things checked directly against the tree show the existing helpers
don't cover this on their own:

- `Resolve(ctx, ref)` (`worktree.go:217-223`) is plain `git rev-parse <ref>` with no `--verify`. Run
  locally: `git rev-parse docs` in this repository exits 0 and prints `docs` — not a sha, not an
  error. `rev-parse` treats an unrecognised argument as a literal token when nothing else matches
  rather than failing, so `Resolve` is not the "reusable as-is" ref→sha function the first draft of
  this doc called it. A ref field that echoes back whatever garbage was typed into it, successfully,
  is worse than one that errors.
- `resolves(ctx, ref)` (`worktree.go:554`) does the right check — `git rev-parse --verify --quiet
  ref^{commit}` — but only returns a `bool`. It throws away the sha it just proved exists, so a
  caller still has to make a second, unguarded call (`Resolve`) to get it — reintroducing the exact
  problem the verify call was supposed to close.

**What it actually costs.** One new function, not two calls chained: `resolveCommit(ctx, ref) (sha
string, err error)` runs `git rev-parse --verify --quiet --end-of-options ref^{commit}` (the
`--end-of-options` marker is git's own documented answer to a ref string that starts with a dash)
and returns its stdout directly as the sha on success. `Tree`, `Blob` and anything else that takes a
client-supplied `ref` call this one function and use the sha it returns for every subsequent git
invocation in that request — never the original ref string again, and never a second resolve. An
operational git error (repository corruption, a timeout) still needs to read as one, not get folded
into "no such revision" — `resolveCommit` distinguishes exit 1 with empty stderr (no such revision)
from anything else, the same distinction `resolves` already draws internally.

### 3. A file read stats before it reads — not `LoadFile`'s pattern

**Why.** `load()`'s `git show sha:path` reads the whole blob into memory before comparing `len(body)`
to `maxBytes`. Safe today because it only ever runs on `isDoc()` files (`.md`, `.txt`) within the
first `eagerFiles = 30` of one commit's changes — a small, implicitly-bounded population. Code
explorer has no such bound: any file, any ref, directly addressable by URL.

**What it actually costs.** `Blob` does two git invocations against the sha `resolveCommit` already
produced: `git cat-file -s <sha>:<path>` for the size (git answers this without reading the object's
content — confirmed locally, `118882` back for a 116 KB file with no content read), and only if that
size is within `maxBytes`, `git show <sha>:<path>` for the bytes, read byte-preserving per decision 1.
Binary detection: sniff the first few KB for a NUL byte, the same heuristic git's own diff machinery
uses. Two processes per open file is fine — this is one file, opened because a person clicked it, not
a thirty-file eager batch.

### 4. Browsing pins to the resolved commit sha, not the moving ref name, once navigation starts

**Why.** A ref picker showing "main" is showing a name that can move mid-session. Without pinning,
clicking into a directory and then opening a file inside it are two separate resolutions of "main,"
and they can disagree. This is the first surface in the codebase that lets a person browse a name
that moves rather than a stored, pinned commit.

**What it actually costs.** `Tree` and `Blob` responses both carry the sha `resolveCommit` produced
for the request. The frontend keeps that resolved sha in navigation state and the URL from the first
response onward, re-resolving the ref name only on an explicit re-pick. A shared link shows "main @
a1b2c3d" rather than a bare branch name — the useful side effect of the same mechanism.

### 5. New routes: project-scoped, lazy per directory

```
GET  /api/projects/{id}/refs                      → Ref[]
GET  /api/projects/{id}/tree?ref=<ref>&path=<dir>  → { resolvedSha, entries: TreeEntry[] }
GET  /api/projects/{id}/file?ref=<ref>&path=<file> → Blob (+ resolvedSha)
```

**Why lazy per directory, and the exact git form.** `git ls-tree <ref> <dir>` — the form the first
draft of this doc named — does **not** list a directory's children: checked locally,
`git ls-tree HEAD docs` prints one line, the `docs` tree entry itself, not what's inside it. The
form that actually lists children is colon syntax against a resolved sha: `git ls-tree -l -z
<sha>:<dir>` (root is `<sha>:` with nothing after the colon, confirmed locally). `-l` adds size
(blobs get a byte count, trees get `-`); `-z` NUL-delimits records instead of newline-and-tab, which
`nameStatus` (`worktree.go`) already parses this way for the same reason — a filename can itself
contain a newline or a tab. `Tree` parses `-z` output the same way. No recursion (`-r`) and no
content: there's no need for an "eager 30" here, because listing a directory never fetches file
content. Content is fetched exactly once, when a specific file is opened.

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

**What it actually costs.** A size cutoff alone does not bound this: the JS engine works by
converting each TextMate grammar's Oniguruma patterns into native `RegExp` objects, and a native
regex engine backtracking badly is a function of the *pattern* meeting adversarial input, not of
file size — a short, deliberately pathological line can hang a tokenizer that a much longer ordinary
file never troubles. The actual guard is a time budget, not a byte count: highlighting runs off the
main thread in a Web Worker, with a wall-clock deadline (proposed: a few hundred milliseconds); a
worker that doesn't finish in time is terminated and the file falls back to plain monospace text.
The size cutoff stays as a cheap first-pass skip (nothing under a few hundred bytes is worth paying
worker-startup cost for), but it is not what makes this safe — the timeout is.

### 7. A new top-level nav destination, checked at phone width from phase 1

**Why.** `VIEWS` is a flat array and `App.vue` a flat `v-else-if` chain — adding "code" is additive.
But the mobile discipline this codebase holds everywhere (`Board.vue`'s lanes stacking under `sm:`,
`ee1dc4c`'s 390px measurements) is a property of how a view is built, not applied afterward.

**What it actually costs.** Below `sm:`, one pane is visible at a time — ref picker, then tree, then
file — with a back affordance. Above it, three columns. This has to be true from the first phase's
UI; retrofitting it onto an already-shipped desktop-only layout is strictly more work.

### 8. Explain reuses `chat.Manager.AskAndWait` against the existing `reviewChat(projectID)` conversation — but `AskAndWait`'s serialization has to change first

**Why reuse it at all.** A code-explorer-specific agent/worktree/conversation would duplicate exactly
what `reviewChat(projectID)` already does. `askAboutTheChange` and `requestGuide` already prove the
pattern works for "ask AI about code I'm looking at."

**What the first draft of this doc got wrong about contention.** It described `AskAndWait`'s
serialization as per-project. Checked directly: `asking` (`chat.go:110`) is a single `sync.Mutex`
field on `Manager` — one lock for the **entire daemon**, not one per project or per conversation.
`AskAndWait` (`chat.go:810-815`) calls `m.asking.Lock()` unconditionally, *before* the per-conversation
`beginTurn(chatID)` check, and holds it for the whole function — which waits out the agent's full
turn, "tens of seconds" by this codebase's own description elsewhere. Concretely: an explain request
in project B does not get an immediate answer to "is my conversation busy," it waits — with no
`ctx` cancellation honoured during the wait, since `Lock()` doesn't take one — for every other
`AskAndWait` call anywhere in the daemon, in any project, to finish first, and only then finds out
whether *its own* conversation was free the whole time. Also checked directly: `requestGuide`'s
existing "busy" precheck (`settings.go:963`, `chatMgr.Busy(project.ID)`) passes the bare project id,
but `Busy` (`chat.go:177-181`) looks up `m.turns[chatID]` and the actual key `AskAndWait` uses is
`reviewChat(projectID)` = `"review-"+projectID` — these never match, so `requestGuide`'s precheck
never fires and the 409 this doc cited as a working precedent is dead code today. The `ErrBusy` a
person actually sees comes only from `beginTurn` inside `AskAndWait` itself, after the global wait.

**What it actually costs.** This is a real change to `internal/chat/chat.go`, not just new code for
code explorer, and it's a prerequisite for phase 3, not a nice-to-have: replace the single blocking
`asking sync.Mutex` with per-`chatID` admission — a check-and-set against the existing `mu`-guarded
`turns` map (or an equivalent lock-striped-by-key), so two different conversations (two different
projects, or a review question and an explain request that land on two different synthetic chat
IDs) never wait on each other, and a busy conversation is rejected immediately rather than after
queueing behind unrelated work elsewhere in the daemon. Also fix `requestGuide`'s dead precheck to
call `Busy(reviewChat(project.ID))` while this code is being touched, since it's the same bug in the
same function this decision depends on. Surfaced to the person as "busy, try again," not silently
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

### 10. Two prompts for two triggers, not one generalized prompt — and a folder is the same trigger as a file

**Why.** `askPrompt` and `guidePrompt` already model two shapes worth keeping distinct:
`askPrompt` embeds text directly so the agent answers about exactly what's on screen;
`guidePrompt` names a ref/commit and lets the agent read it with its own tools. "Explain this
file" is guide-shaped (name `ref:path`, let the agent `git show` it in its own worktree); "explain
this selection" is ask-shaped (embed the selected lines directly). "Explain this folder" is the same
guide shape as a file, naming `ref:dir` instead of `ref:path` and asking what the directory is for
and how its pieces relate — the agent already has `ls`/`git ls-tree` in its own worktree, so nothing
new is needed server-side to support it, only a third UI trigger next to the tree's folder rows.

**What it actually costs.** Two prompt-building functions, not three — file and folder share the
guide shape and differ only in the noun the prompt names, so it's one function taking either.
Reuses `askTimeout` (3 minutes) as-is.

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
3. **Explain.** `chat.Manager`'s per-`chatID` admission fix (decision 8) lands first, since it's a
   correctness fix independent of the rest of this phase and everything else here depends on it not
   silently degrading to global serialization. Then the `explain`/`explain/{jobId}` routes,
   `AskAndWait` against `reviewChat(projectID)`, the prompt shapes from decision 10, and three UI
   triggers: a persistent "Explain this file" toolbar action, "Explain this folder" on a tree row,
   and a selection popover that appears only when text is highlighted. Demoable: select a few lines,
   click "explain file," or click "explain folder," get an answer — and confirm two explain requests
   in *different* projects no longer wait on each other, only two in the *same* project's review
   conversation do, surfaced as "busy, try again."

Order argument: 2 before 3 because it's the cheaper, purely-additive, zero-new-runtime-surface
change, and it makes 3's UI read better once it exists — not a hard dependency; either could ship
first without breaking the other.

## Acceptance checks

A demo is not the test plan. Each phase's implementation PR runs these against a real project, not
just the happy path a screenshot would show:

- **Exact bytes.** A file with leading blank lines, trailing whitespace, mixed line endings, and one
  with no trailing newline at all — content returned by `Blob` matches the working tree byte for
  byte, and selection line numbers land on the right line in each case. This is what decision 1's
  `git()`-helper finding exists to catch.
- **Unusual filenames.** A path with a space, a unicode character, and one that itself starts with a
  dash — `Tree` lists it correctly and `Blob` reads the right file, not a different one and not a git
  option.
- **A moving ref.** Start browsing a branch, push a new commit to it from another checkout mid-session,
  continue navigating — the session keeps showing the sha it pinned (decision 4), not a mix of old
  and new.
- **Oversized and binary files.** A file just over `maxFile` reports `TooLarge` without its bytes
  ever leaving the daemon process; a binary file reports `Binary` and is never handed to the
  highlighter.
- **Concurrent explains.** Two explain requests in the *same* project's review conversation — the
  second gets `ErrBusy` promptly. Two explain requests in *different* projects, timed to overlap —
  neither waits on the other. Both cases are pass/fail against decision 8's fix, not something a
  single manual click-through would reveal.
- **A dead poll target.** Restart the daemon with an explain job's id still held by a client, then
  poll it — a plain "not found," not a hang or a 500.
- **Cold and warm navigation on a throttled connection at 390px.** Not "it looks fine" — an actual
  measured load time for opening a large directory and a large file, throttled to a realistic
  tailscale-over-phone connection, compared before and after phase 2's highlighter lands.

## Failure modes to design against

- **A huge blob read in full before its size is checked**, by copying `LoadFile`'s pattern instead
  of decision 3's stat-then-read.
- **A ref or path string reaching git's argv as if it were an option.**
- **A tree shown for a ref that has since moved or been deleted** between clicks — bounded by
  sha-pinning, but the first resolution is still a race with whoever pushes next; accepted.
- **A pathological file hanging the highlighter or the tab** — guarded by a worker time budget
  (decision 6), not a size cutoff; size alone does not bound a regex engine's worst case.
- **Repo content rendered as live DOM instead of inert text.** An HTML or SVG file in the repo is
  the same threat `artifacts.go`'s allowlist exists to contain; never `v-html` or iframe repo bytes
  without a sandbox.
- **An explain request queuing invisibly behind unrelated work anywhere else in the daemon** — the
  literal behavior of `AskAndWait`'s serialization before decision 8's fix, and read by the person as
  "the button did nothing" rather than "busy." Decision 8's per-`chatID` admission is what closes
  this, not a UI spinner over the existing wait.
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
