# Architect orchestration

A plan under argument, not a description of anything that exists. Issue [#40].

One card gets a switch that turns it from a unit of work into a feature: the architect splits it,
creates the subtasks, orders them, supervises them, and reviews the whole before a person lands it.

Eight decisions were settled on 2026-09-04. A review the same day found six problems with the first
draft: two broke a decision, one was a false claim about git, and the phase order shipped an
invariant it could not hold. What survived, what changed, and what the draft got wrong are all
recorded below, because the correction is the more useful half.

[#40]: https://github.com/kconfesor/zerg/issues/40

## What the first draft got wrong

Kept rather than quietly fixed, in the spirit of ARCHITECTURE 6.1.

- **"A different string, not new git machinery" was false.** `Git.Merge` refuses unless the target
  branch is *checked out at the repository it runs in*, and integrates with `merge --ff-only`.
  Independent subtasks do not fast-forward onto a shared branch, and the project root can hold only
  one branch checked out, so two features could never integrate at once. See decision 1.
- **The phase order shipped a broken invariant.** Phases 2 and 3 created executable subtasks while
  landing was still phase 4, and an ungated terminal completion applies the project's integration
  policy immediately: `merge` puts half a feature on base, `pr` opens one pull request per subtask,
  `branch` leaves the work on reused role branches. See "Phases".
- **Inheriting supervision would have demanded a human approval per subtask.** `Send` holds a
  completion whenever `sender.Gate == GateApproval || task.Supervised`, and `decide` refuses a
  terminal approval from anyone but the operator. That is the opposite of the point. See decision 3.
- **"No chance of the two disagreeing" was false.** The plan rows are a SQLite write and the prose
  is a git commit, and nothing binds them. See decision 8.

## What already exists to build on

Facts, checked against the tree rather than remembered.

| | |
|---|---|
| `tasks.supervised` | The per-card switch precedent, schema 37. A property of the card, not of a role's gate, so it does not move when a role is skipped |
| `purpose=supervisor` | The architect sidecar. Started and retired by the daemon, never on the board |
| `messages.priority` | Default 50, claims order by `priority ASC, created_at ASC`. Priority lives **on the message**, and is hardcoded to 50 when a card opens, when it completes and when it is rejected |
| `projects.integration` | `merge`, `branch` or `pr`, applied the moment a terminal completion happens |
| `Git.Merge` | **Refuses unless the target branch is checked out where it runs, and is `--ff-only`.** Not a general "merge into any branch" |
| `Git.MergeInto` | Merges a handed-over commit into the claiming role's worktree, which is how a role receives prior work |
| `TaskBranchPrefix = "zerg/"` | Per-task branches already exist for pull requests, precisely because "a role branch accumulates everything that role has ever done" |
| `hatchery.EnsureWorktree` | Returns an existing worktree untouched; `-B` resets the role branch **only on first creation**, so a role branch accumulates work across cards |
| `Onward` | Decides terminality as "no recipient named, and this sender is last in the routed team" |
| `rework_count`, `ListReworkedTasks` | Cards that keep going backward, with an operator threshold, surfaced in Attention |

Missing, and roughly the size of the work:

- No relation between cards, no dependencies, no priority on a task, and no agent verb that creates
  work. The socket has `next`, `done`, `send`, `ask`, `artifact`, `remember`, `approve`, `reject`,
  `answer`.
- `tasks.state` is `CHECK (state IN ('queued','working','done','rejected'))`, which cannot express
  planning, blocked, integrating, in review, remediating or cancelled.
- `StopTask` and `DeleteTask` act on one task id and know nothing about descendants.
- One worktree and one process per role, with leases, sessions, quotas, nudges and silence watching
  all keyed by role name.

## Decisions

### 1. Subtask work lands on a per-feature branch, integrated in its own worktree

`zerg-feature/<feature-id>` is created when the plan is approved. Subtasks integrate into it, and
the feature lands on base as one thing after the architect's review and the operator's approval.

**Why.** The point of reviewing a whole is being able to refuse it. Merging each subtask to base as
it finishes makes the review advisory, and leaves half a feature on base when it says no.

**What it actually costs**, corrected from the first draft:

- **A dedicated integration worktree per feature.** `Git.Merge` requires the target branch to be
  checked out where it runs, and the project root holds the base branch. Integrating at the root
  would move the root's HEAD and would serialise every feature in the project.
- **Real merges, not fast-forwards.** `--ff-only` is right for today's linear one-card-at-a-time
  flow. Two subtasks branched from the same base do not fast-forward onto each other, so feature
  integration needs a path that can produce a merge commit and report a conflict as an
  operator-fixable error rather than a 500.
- **Persisted `base_sha`, `head_sha` and branch name per feature,** so integration is idempotent
  after a crash, a review can name the exact head it reviewed, and a refresh from base is a recorded
  event rather than a guess.
- **The feature id in the branch name, not the slug.** Names are unique per project but they are
  prose, and two features can slugify to the same string.

### 2. A feature is a row in `tasks`, and its lifecycle is not in `tasks.state`

A `kind` of `work` or `feature` and `parent_id` on subtasks give the board, the trail, history, the
cost view, hidden, pinned, delete-cascade and rework counting for free. A feature has no route, and
`Claim` selects `FROM routes JOIN messages`, so it can never be handed to a role.

**But `kind` plus `parent_id` is not a lifecycle.** `tasks.state` is a four-value `CHECK`, and a
feature moves through planning, plan approval, blocked execution, integration, integration conflict,
review, remediation, cancellation and landing. Bending those into `queued/working/done/rejected`
makes the state column mean something different depending on `kind`, which reads fine and is wrong
in whichever query somebody writes next.

**Companion tables instead**, so `tasks` keeps its meaning:

- `feature_runs`: one live run, its branch, `base_sha`, `head_sha`, and its own state.
- `feature_plan_revisions`: immutable. A rejected plan produces a new revision rather than an edit,
  which is what makes decision 9 enforceable rather than a convention.
- `feature_plan_items`: the subtasks as planned, each referencing the child task it produced.
- `feature_deps`: edges between plan items.
- `feature_reviews`: the architect's verdict, bound to the head it reviewed.

**What it still costs.** Every query that lists cards for a person must exclude features. The audit
is `ListTasks`, the board query, history paging, `ListReworkedTasks` and the attention queries, and
it wants a test asserting a feature never appears in a lane.

### 3. A subtask's completion is not a land, so it is not terminal

**The problem.** Supervision was to be inherited, and a supervised card's completion is held for a
human while `decide` refuses a terminal approval from anyone but the operator. Inheriting
supervision would therefore demand an operator approval for *every subtask*.

**The resolution, which relaxes nothing.** Terminality is computed by `Onward` as "no recipient
named, and this sender is last in the routed team". Inside a feature the last role does have
somewhere to send: the feature. A subtask's final handoff is a handoff into feature integration, not
a land, so it is not terminal and the operator-only check never fires on it. The only terminal event
in a feature is the feature landing on base, which stays human exactly as #38 left it.

The architect decides a subtask's integration the way it already decides a mid-pipeline gate. A new
verdict type plus a weakened terminal check was the alternative and is rejected: loosening the check
that makes "the last click stays human" true is not worth the convenience.

### 4. The architect may reject a feature, and may not approve one

Its review is written as evidence on the final approval, bound to the head sha it reviewed. It may
send a feature back on its own authority; approving stays human.

**A review is about a head, not about a feature.** Any integration, base refresh or plan revision
after it invalidates it, because the thing reviewed no longer exists. Without that binding the
review is a claim about a moving target, which is the class of bug ARCHITECTURE 6.1 collects.

### 5. A dependency is satisfied by integration, not by a child saying "done"

A dependent needs its dependency's *work*, and a role receives work through `MergeInto` when it
claims a message carrying a commit. A dependency marked done whose commit is not yet in the feature
head hands the dependent an envelope pointing at a tree that does not contain what it depends on.

So a dependency is satisfied when its commit is integrated into the feature head, and the route that
unblocks a dependent carries the **feature head sha** rather than the dependency's own commit, so
the dependent receives everything it depends on rather than one piece of it.

Integration, dependency release and the creation of newly-ready routes happen in one transaction. A
crash between them leaves a feature either advanced or not, never half.

Edges are validated within one feature, self-edges and duplicates refused, and the whole DAG checked
for cycles at write time. A cycle is work that can never start while looking queued, which is the
quiet failure this project keeps finding.

### 6. Priority needs one source that survives a handoff

`messages.priority` is the only priority the queue reads, and it is hardcoded to 50 when a card
opens, when it completes and when it is rejected. A priority set at planning time is lost at the
first handoff unless it is carried deliberately.

A plan item carries a priority, the child task carries it, and every message the daemon writes for
that child derives its priority from the task rather than from the constant. `blocked` is a state
distinct from `queued`, because a queued card is one a role will pick up and a blocked card is one
nothing will, and the board must not draw them the same.

### 7. Nothing is created until the operator approves the plan

The approval states how many subtasks and what they are likely to cost; `usage_turns` holds enough
per-role history for that estimate to be computed rather than guessed. Creating the subtasks is the
step that spends the money and cannot be undone with a button.

Rejected: a bare cap on subtask count, which says nothing about whether the split was right.

### 8. The plan is rows for the queue and prose for the reader, bound by a digest

Subtasks, dependencies and priorities are rows because the queue must read them. The reasoning is a
document committed to the repository the way `docs/zerg/<slug>/decisions.md` already is.

**The first draft claimed they cannot disagree. They can:** one is a SQLite write and the other a
git commit. What the split actually buys is that neither is bent to serve the other. Keeping them in
step needs a digest of the plan rows on the revision, and the prose commit recorded beside it the
way `evidence_sha` already records a decision's commit, so a plan whose rows moved after the prose
was written is detectable rather than merely unlikely.

### 9. The plan is accepted or rejected, not edited

Rejecting carries a note and produces a new immutable revision. One author for the plan, so the
review measures work against something the architect actually reasoned about.

If a whole re-planning turn for one unwanted subtask proves to be the common case, the escape hatch
is to allow editing *and* hand the architect the diff, so its review measures against the edited
plan.

### 10. A failing subtask stalls the feature, and the operator has named actions

Stalling reuses the rework threshold that already surfaces a card going backward too often.
Automated re-planning is rejected: it turns one bad subtask into several new ones and spends money
doing it.

**Stalling alone is not a contract.** The operator needs named actions: retry the child, waive a
dependency with a rationale, approve a repair revision, or cancel the feature. Cancellation is soft,
and deleting a live feature hierarchy is refused rather than cascaded, because `DeleteTask` acts on
one id and cascading it across running children would leave agents writing to rows that no longer
exist. Every late agent write re-checks that its feature is not cancelled and that its plan revision
is still current.

### 11. Parallelism is whatever already exists, for now

Independent subtasks in different roles already run at once. Per-task worktrees and several
processes per role become their own issue: leases are keyed `(project, role)`, `role_sessions` by
`(project, role, harness, fingerprint)`, `s.roles` is a map by role name, and `nudgeIdle`,
`watchForSilence`, quota reporting and `Status` all key by role.

**Deferring isolation has a cost the first draft missed.** Role worktrees are reused and role
branches accumulate work across cards, since `EnsureWorktree` resets with `-B` only when it creates
the worktree. A subtask's commit can therefore contain another card's work, and a batch claim
distinguishes priority and skip but not feature membership. Until per-task worktrees exist, either a
role's worktree is reset to the feature head before a feature's subtask runs, or batches are
constrained so one claim never mixes features. Neither is free, and this is the argument for
bringing isolation forward rather than leaving it last.

## Phases

Revised: the first draft had subtasks executing two phases before there was anywhere for their work
to go.

1. **Relation and a visual.** A feature groups cards; the board shows which. No architect, nothing
   executes. Answers the badge, colour or grouping question by looking at it in a browser.
2. **An inert plan.** The split verb behind a new capability, the plan rows and prose, the operator
   approval with count and estimate, and the immutable revision. **No child tasks, no routes.** A
   plan that can be produced, read, approved and rejected is worth having on its own, and it is the
   whole of the expensive-decision surface.
3. **Materialisation, integration and scheduling, together.** Child tasks, the feature branch and
   its integration worktree, dependency release on integration, blocked state, priority that
   survives a handoff, and non-terminal child completions. One phase, because any subset of them
   puts work in the wrong place.
4. **Feature review and the land.** The review bound to a head sha, the operator's approval, the
   merge to base, and the remediation actions in decision 10.
5. **Per-task worktrees**, unless decision 11's constraints have already forced them earlier.

## Failure modes to design against

- **A feature reports done because every subtask is done.** Nothing checked the whole, and "done"
  was derived from counting children. The likeliest thing to ship broken.
- **A review that outlived what it reviewed**, because a refresh or a late integration moved the
  head after the architect looked at it.
- **A subtask commit containing another card's work,** because the role branch was never reset
  between features.
- **A dependency satisfied by a state column** while the dependent's worktree does not contain the
  work, so it reimplements or overwrites it.
- **A dependency cycle, or a dependent waiting on a dropped subtask,** sitting in a queue nobody is
  reading and indistinguishable from work waiting its turn.
- **A cancelled feature whose children are still running,** writing to rows that were deleted.
- **Ten pipelines discovered as a bill** rather than as an estimate before the split.

## Still open

- Whether the plan approval is an `approvals` row. Reusing them brings Attention, the decision
  panel, `decided_by`, `evidence_sha` and the deciding model, which all fit. An approval is tied to
  a message and a route today, and a plan is tied to neither. Phase 2.
- How the feature branch is refreshed from base during a long feature, and what that does to a
  review taken before the refresh. Phase 4.
- Whether a subtask can itself be a feature. No, until something demands otherwise: a plan that
  plans is much harder to estimate, and the estimate is what makes decision 7 work.
- How a feature interacts with `skip` and `deploy`, which are per card today.
- What the board actually shows. Phase 1 exists to answer it by looking, with a number read back out
  of a browser rather than reasoned about here.
