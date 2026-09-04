# Architect orchestration

A plan under argument, not a description of anything that exists. Issue [#40].

One card gets a switch that turns it from a unit of work into a feature: the architect splits it,
creates the subtasks, orders them, supervises them, and reviews the whole before a person lands it.

Eight decisions were settled on 2026-09-04 and are recorded below with what each one rules out, so
a later reader can see the alternative rather than only the choice. What remains open is at the
bottom. Nothing here is built yet.

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

## Decisions

### 1. Subtask work lands on a per-feature branch

`zerg-feature/<slug>` is created when the plan is approved. Subtasks merge into it as they finish,
and the feature lands as one thing after the architect's review and the operator's approval.

**Why.** The point of reviewing a whole is being able to refuse it. Merging each subtask to base as
it finishes makes the review advisory: it can say "that went wrong" and not "do not ship this", and
a rejected review leaves half a feature on the base branch.

**Why it is affordable.** `Integrator.Merge(ctx, path, baseBranch, commit)` already takes the target
branch as a parameter, and `projects.integration` already has a `branch` mode meaning "the task is
finished, landing it is someone else's decision". This is a different string and one branch to
create and land, not new git machinery.

ARCHITECTURE 9.2 bans integration policy *per role*, because disabling a role moves the policy to
whichever role happens to be last. A per-feature target does not have that failure: a feature is a
unit with a start and an end, and the policy is attached to the thing being landed.

**What it costs.** The feature branch diverges from base while a long feature runs, so subtasks
conflict with work that landed meanwhile. Needs a refresh story, and argues for keeping features
short.

### 2. A feature is a row in `tasks`

A `kind` of `work` or `feature`, and `parent_id` on subtasks.

**Why.** The board, the trail, history, the cost view, hidden, pinned, delete-cascade, the
supervised switch and rework counting all come free. The obvious danger does not exist: `Claim`
selects `FROM routes JOIN messages` with `tasks` only left-joined for `skip`, so a feature row,
which has no route, can never be handed to a role.

**What it rules out.** A separate `features` table, which would need a second implementation of the
history, spend and trail views.

**What it costs.** Every query that lists cards *for a person to look at* must exclude features, or
a card appears that nobody can work. The audit is `ListTasks`, the board query, history paging,
`ListReworkedTasks` and the attention queries, and it wants a test asserting a feature never appears
in a lane. `tasks.lane` is `NOT NULL`, so a feature needs a lane value no role owns, and `state` has
a `CHECK`, so per AGENTS.md that means adding a column rather than rebuilding the table.

### 3. The architect may reject a feature, and may not approve one

Its review is written as evidence on the final approval, so the operator reads the verdict and the
reasoning before landing. It may send a feature back on its own authority. Approving stays human.

**Why.** #38 already ruled that a supervisor cannot answer its own question, and that the land stays
with a person. Reviewing work against a plan it wrote is the same shape one level up, so the review
is a report rather than a gate it can pass by itself. The asymmetry earns its keep: a feature that
is wrong should not wait for a person to be told so.

**What it rules out.** A second architect on a different model, which would be genuinely independent
and would double the cost of the most expensive step.

### 4. A failing subtask stalls the feature and says so

The feature stops advancing and appears in Attention, reusing the rework threshold that already
surfaces a card going backward too often.

**What it rules out.** Automated re-planning, which recovers without the operator and turns one bad
subtask into several new ones while spending money doing it. Revisit when there is evidence about
how often this happens, rather than before.

### 5. Nothing is created until the operator approves the plan

The approval states how many subtasks and what they are likely to cost. `usage_turns` holds enough
per-role history for that estimate to be computed rather than guessed.

**Why.** Creating the subtasks is the step that spends the money and cannot be undone with a button.
It is the only irreversible moment in the flow and therefore the only place a gate belongs.

**What it rules out.** A bare cap on subtask count, which is blunt: eight badly chosen subtasks cost
what eight good ones do, and a cap says nothing about whether the split was right.

### 6. Parallelism is whatever already exists, for now

Independent subtasks in different roles already run at once. Per-task worktrees and several
processes per role become their own issue.

**Why.** Everything else here is useful without it, and the cost is not mainly the worktrees.
`hatchery.Path` takes a name and chat already derives one per conversation, so that half is
precedented. The rest is not: leases are keyed `(project, role)`, `role_sessions` by
`(project, role, harness, fingerprint)`, `s.roles` is a map by role name, and `nudgeIdle`,
`watchForSilence`, quota reporting and `Status` all key by role. Making a role plural changes that
key nearly everywhere, and N processes per role multiplies token spend and machine load as well as
code.

**Measure first.** Whether cross-role parallelism is enough is an empirical question and the answer
should come from a real feature rather than from this document.

### 7. The plan is rows, with the reasoning in prose beside it

Subtasks, dependencies and priorities are rows, because the queue has to read them. The architect's
reasoning is a document committed to the repository, the way `docs/zerg/<slug>/decisions.md` already
is.

**Why.** The queue needs something unambiguous and the operator needs an argument. Splitting them
means neither is bent to serve the other, and there is no chance of the two disagreeing about what
the work actually is.

**What it rules out.** Parsing subtasks out of prose an agent wrote, where a malformed plan becomes
a malformed queue.

### 8. The plan is accepted or rejected, not edited

Rejecting carries a note, and the architect re-plans against it.

**Why.** One author for the plan, so the feature review measures the work against something the
architect actually reasoned about. An edited plan has two authors and the review then measures
against something its writer did not entirely write.

**What it costs.** A whole re-planning turn when the operator only wanted to drop one subtask. If
that turns out to be the common case, the escape hatch is to allow editing *and* hand the architect
the diff, so its review measures against the edited plan.

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
5. **Real parallelism.** Per-task worktrees and several processes per role, as its own issue, and
   only once a real feature has shown that cross-role parallelism is not enough.

The order is deliberate. Phase 1 is useful with no architect involvement at all, and phases 2 and 3
are useful before anything lands differently, so the branch that changes how work reaches the base
branch is the fourth thing built rather than the first.

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

## Still open

Design questions the decisions above did not settle, and which the phase that meets them should
answer rather than this document.

- **Is the plan approval an `approvals` row?** Reusing them would bring Attention, the decision
  panel, `decided_by`, `evidence_sha` and the model that decided, all of which fit. An approval is
  tied to a message and a route today, and a plan is tied to neither, so this is a real question and
  not a formality. Phase 2.
- **How the feature branch is refreshed from base** while a long feature runs. Phase 4, and it is
  the cost decision 1 accepted.
- **Whether a subtask can itself be a feature.** Recursion is a real question. The answer is no
  until something demands otherwise, because a plan that plans is much harder to put a cost estimate
  on, and the estimate is what makes decision 5 work.
- **How a feature interacts with `skip` and `deploy`,** which are per card today. Probably inherited
  by subtasks from the feature, which is the same shape as supervision.
- **What the board actually shows.** Badge, colour, a group under the feature name, or a separate
  view of the plan as a graph. Deliberately unanswered: phase 1 exists to answer it by looking at
  it, in a browser, with a number read back out rather than reasoned about.
