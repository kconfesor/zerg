# Architect orchestration

A plan under argument, not a description of anything that exists. Issue [#40].

One card gets a switch that turns it from a unit of work into a feature: the architect splits it,
creates the subtasks, orders them, supervises them, and reviews the whole before a person lands it.

This document exists to settle six decisions before any of it is built, because five of them are
cheap to argue about now and expensive to change afterwards. Each one lists what it turns on, the
options, a recommendation, and what the recommendation costs. Argue with the recommendations; they
are starting positions, not conclusions.

[#40]: https://github.com/kconfesor/zerg/issues/40

## What already exists to build on

Facts, checked against the tree rather than remembered.

| | |
|---|---|
| `tasks.supervised` | The per-card switch precedent, schema 37. A property of the card, not of a role's gate, so it does not move when a role is skipped |
| `purpose=supervisor` | The architect sidecar. Started and retired by the daemon, never on the board, decides mid-pipeline gates and questions |
| `messages.priority` | Default 50, claims order by `priority ASC, created_at ASC`. Priority exists, on the message |
| `projects.integration` | `merge`, `branch` or `pr`. **`branch` already means "finish the task, land it later"** |
| `Integrator.Merge(ctx, path, baseBranch, commit)` | The target branch is a parameter, not a constant |
| `hatchery.Path(name)` | Keyed by a name, not by a role. Chat already derives one per conversation |
| `rework_count`, `ListReworkedTasks` | Cards that keep going backward, with an operator threshold, surfaced in Attention |
| `docs/zerg/<slug>/decisions.md` | Where the architect already writes its reasoning, committed and linked from the trail |

And what is missing, which is the size of the work:

- No relation between cards. `tasks` has no parent, no group, no tag.
- No dependencies, and nothing that holds work out of a queue until it can start.
- No priority on a task, only on a message.
- No agent verb that creates work. The socket has `next`, `done`, `send`, `ask`, `artifact`,
  `remember`, `approve`, `reject`, `answer`.
- One worktree and one process per role, and leases, sessions, quotas, nudges and silence watching
  all keyed by role name.

## The six decisions

### 1. Where does subtask work land?

**Turns on:** whether the feature review can say "do not ship this" or only "that went wrong".

- **(a) Each subtask merges to base as it finishes.** Today's behaviour, no new machinery. The
  feature review happens after every piece is already on the base branch, so it is advisory, and
  "half a feature on main after the review rejects the other half" is a live outcome.
- **(b) Subtasks merge into a per-feature branch, which lands once.** `zerg-feature/<slug>` is
  created when the plan is approved, subtasks merge into it instead of into base, and the feature
  lands as one commit after the architect's review and the operator's approval.

**Recommendation: (b).** The point of reviewing a whole is being able to refuse it, and (a) cannot.
Mechanically it is cheaper than it sounds: `Merge` already takes the target branch as a parameter,
so this is a different string plus creating and landing one branch.

ARCHITECTURE 9.2 says integration is per project and *deliberately not per role*, because attaching
the policy to a role means disabling that role moves it. A per-feature target does not have that
failure: a feature is a coherent unit with a start and an end, and the policy is attached to the
thing being landed rather than to a role that might be skipped.

**What it costs:** the feature branch diverges from base while a long feature runs, so subtasks
conflict with work that landed meanwhile. Needs an answer for refreshing the branch from base, and
it argues for keeping features short.

### 2. Is a feature a task?

**Turns on:** how much existing surface comes free, against how many queries have to learn a new
exception.

- **(a) A row in `tasks` with a `kind` of `work` or `feature`, and `parent_id` on subtasks.** Free:
  the board, the trail, history, the cost view, hidden, pinned, delete-cascade, the supervised
  switch, rework counting. A feature is never routed, so `Claim` will never hand it to anyone, since
  claiming selects through messages and routes rather than through tasks.
- **(b) A separate `features` table with `tasks.feature_id`.** No risk of a feature turning up in a
  lane. Costs a second implementation of history, cost and trail views.

**Recommendation: (a), with an audit as part of the work.** The free surface is large and the danger
is specific rather than diffuse: every query that lists cards for a person to look at has to exclude
features, or a card appears that nobody can work. The call sites to check are `ListTasks`, the board
query, history paging, `ListReworkedTasks` and the attention queries. That list is short enough to
be checked once and covered by a test that asserts a feature never appears in a lane.

**What it costs:** `tasks.lane` is `NOT NULL` and `state` has a `CHECK` constraint. A feature needs a
lane value no role owns, and its lifecycle is not the same as a card's. Per AGENTS.md, a `CHECK` in
the way means adding a column rather than rebuilding the table.

### 3. Can the architect review a plan it wrote?

**Turns on:** whether the feature review is a gate or a report.

The precedent is already set twice: `NextDecision` refuses to offer the supervisor a question it
asked itself, and a terminal completion is refused for anyone but the operator. Reviewing work
against a plan it wrote is the same shape one level up.

- **(a) The review is a report attached to the land.** The architect produces a verdict and its
  evidence; the operator sees both at the final approval and decides. Consistent with the existing
  rule, no new machinery.
- **(b) A second architect, different role or model, reviews.** Genuinely independent, doubles the
  cost of the most expensive step, and needs a second sidecar.
- **(c) Asymmetric: the architect may reject the feature but not approve it.** Rejecting sends work
  back without waiting for a person, which is a real saving on the common case; approving stays
  human.

**Recommendation: (a) plus (c).** They compose: the architect writes its review as evidence, and may
send the feature back on its own authority, but the land stays where #38 put it. The asymmetry is
worth having because a feature that is wrong should not wait for a person to be told so.

### 4. What happens when a subtask goes wrong?

**Turns on:** whether one bad subtask stalls a feature, re-plans it, or is dropped.

- **(a) The feature stalls and the operator is told.** The rework threshold and Attention already do
  exactly this for a single card.
- **(b) The architect re-plans: edits, replaces or adds subtasks.**
- **(c) The subtask is dropped, and its dependents are cancelled or unblocked.**

**Recommendation: (a) first, and resist (b).** Automated re-planning turns one bad subtask into
several new ones, spends money doing it, and is the hardest thing on this list to reason about when
it goes wrong. Surface it, let a person decide, and revisit once there is evidence about how often
it happens.

### 5. What stops a split producing thirty subtasks?

**Turns on:** whether the expensive irreversible step has a gate.

**Recommendation: the operator approves the plan before any subtask exists**, and the approval
screen states the count and an estimate. `usage_turns` has enough history to estimate a per-role
average, so "9 subtasks, roughly 40 to 70 dollars" is answerable rather than guessed. A hard cap is
a blunt second line and can wait.

This is the single most important gate in the design. Creating the subtasks is the step that spends
the money and is not undoable by pressing a button.

### 6. How parallel is parallel?

**Turns on:** how large this feature actually is. This is the piece that makes it the biggest thing
on the roadmap.

Today a role has one worktree and one process, and a lease is held per role, so two cards in one
lane are worked one after the other. Parallelism across subtasks is therefore only real where the
subtasks sit in different roles at the same time.

- **(a) Take the parallelism that exists.** Independent subtasks in different roles already run at
  once. Free, and possibly enough.
- **(b) Per-task worktrees and more than one process per role.** `hatchery.Path` takes a name and
  chat already derives one per conversation, so the worktree half is precedented. The rest is not:
  leases are keyed `(project, role)`, `role_sessions` by `(project, role, harness, fingerprint)`,
  `s.roles` is a map by role name, and `nudgeIdle`, `watchForSilence`, quota reporting and `Status`
  all key by role. Making a role plural means changing that key nearly everywhere.

**Recommendation: (a) first, and treat (b) as its own project with its own issue.** Everything else
in this document is useful without it. Measure whether cross-role parallelism is enough before
paying for the rest, and note that N processes per role multiplies token spend and machine load as
well as code.

## Proposed phases

Each ships something usable on its own, which is the only way something this size gets built without
a branch that lives for a month.

1. **Relation and a visual.** A feature groups cards; the board shows which cards belong to it. No
   architect involvement at all. This alone removes the "held in the operator's head" problem, and
   it is where the badge, colour or grouping question gets answered by looking at it.
2. **The split.** A new verb behind a new capability, granted explicitly the way `CanDecide` is and
   never inherited by pipeline tokens. A plan artifact, an operator approval with a count and an
   estimate, and subtasks created in one transaction. Names must not collide: `idx_tasks_name` is
   unique per project.
3. **Dependencies and priority.** A DAG between subtasks, priority on the task, and a queue that
   holds a blocked subtask out of its lane until its dependencies are done. Cycles refused at write
   time, because a cycle is work that can never start while looking queued.
4. **Feature branch and the land.** Decision 1, plus the architect's feature review as evidence on
   the final human approval.
5. **Real parallelism.** Decision 6b, if it is still wanted by then.

## Failure modes to design against

The quiet ones, in the spirit of ARCHITECTURE 6.1, where a value that names an outcome is derived
from a proxy for it.

- **A feature reports done because every subtask is done.** Nothing checked the whole, and "done"
  was derived from counting children rather than from the feature working. This is the failure this
  entire feature is most likely to ship with.
- **A dependency cycle, or a subtask blocked on one that was deleted.** Work sitting in a queue
  nobody is reading, indistinguishable from work that is merely waiting its turn.
- **Half a feature on the base branch** after the review rejects the other half. Decision 1 is the
  answer to this one.
- **The plan and the subtasks drift apart**, so the feature review measures the work against a plan
  that no longer describes it.
- **Ten pipelines discovered as a bill** rather than as an estimate before the split.
- **A subtask that was skipped or dropped leaves its dependents queued forever**, because "ready"
  was computed from "no unfinished dependencies" and a dropped dependency is neither finished nor
  unfinished.

## Not yet argued

- What the plan artifact actually is. A markdown document in the repository, a structured row, or
  both. It has to be reviewable by a person at the approval gate and readable by the architect at
  the review, and those want different shapes.
- Whether a subtask can itself be a feature. Recursion is a real question and the answer is probably
  no, at least at first.
- How a feature interacts with `skip` and `deploy`, which are per card today.
- Whether the operator can edit the plan at the approval gate, or only accept and reject it.
