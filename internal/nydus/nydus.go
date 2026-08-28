// Package nydus routes work between roles.
//
// Everything here is a transaction against SQLite, which is the point.
// Coordinating through files across N worktrees makes delivery a loop of copies
// that can half-complete, claiming a check-then-move with no lock, and a lost
// wake-up a permanent stall with no timer and no retry. Those are not bugs in
// such a design so much as the design.
package nydus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// DefaultLease is how long a role may hold work before it returns to the queue.
const DefaultLease = 20 * time.Minute

// Integrator moves commits between trees: a hand-off commit into the recipient's
// worktree, and the terminal role's commit into the project's base branch.
// The orchestrator owns integration, not whichever agent happened to be last.
type Integrator interface {
	Merge(ctx context.Context, projectPath, baseBranch, commit string) error

	// MergeInto brings commit into the working tree at path. Called when work
	// is claimed, so a role opens its tree and finds the work already there.
	MergeInto(ctx context.Context, worktreePath, commit string) error

	// Publish pushes a branch at commit and opens a pull request, returning its
	// URL. Used when a project integrates by PR rather than by merging.
	Publish(ctx context.Context, repoPath, base, commit, title, body string, draft bool) (string, error)

	// Landed reports whether an integration that may have been interrupted
	// actually completed: the commit already contained in the base branch, or
	// the pull request already open. It is what lets a crash mid-integration be
	// reconciled from the repository rather than guessed at.
	// Landed says whether the work is already integrated and where it went.
	// The second answer matters for a gated pull request: its note was written
	// before anyone approved it, so the URL exists only here.
	Landed(ctx context.Context, repoPath, base, commit, title, mode string) (string, bool, error)

	// Resolve turns a commit-ish into the absolute sha it names in the tree at
	// path. Every commit that enters the system goes through this.
	Resolve(ctx context.Context, worktreePath, ref string) (string, error)
}

// Nydus is the transport. It is safe for concurrent use: every operation that
// changes queue state does so inside one transaction.
type Nydus struct {
	db         *store.DB
	integrator Integrator
	onTaskDone func(ctx context.Context, projectID, taskID, commit string)
	leaseFor   time.Duration
	now        func() time.Time
}

type Option func(*Nydus)

// WithLease overrides the lease duration.
func WithLease(d time.Duration) Option { return func(n *Nydus) { n.leaseFor = d } }

// WithClock replaces the clock, so lease expiry is testable without sleeping.
func WithClock(f func() time.Time) Option { return func(n *Nydus) { n.now = f } }

// WithIntegrator supplies the merge strategy for terminal completion.
func WithIntegrator(i Integrator) Option { return func(n *Nydus) { n.integrator = i } }

// WithOnTaskDone registers what to run once a task lands in Done.
//
// A callback rather than nydus doing the work itself: reclaiming disk needs
// worktrees and settings, and a router that reached for those would depend on
// half the system to move a message. It runs after the transaction commits and
// its failure never affects the completion, because a task is finished whether
// or not the tidying afterwards worked.
func WithOnTaskDone(fn func(ctx context.Context, projectID, taskID, commit string)) Option {
	return func(n *Nydus) { n.onTaskDone = fn }
}

func New(db *store.DB, opts ...Option) *Nydus {
	n := &Nydus{
		db:       db,
		leaseFor: DefaultLease,
		now:      func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// ── starting work ─────────────────────────────────────────────────────────

// NewTask opens a card and queues it for the first enabled role.
func (n *Nydus) NewTask(ctx context.Context, projectID, name, body string) (*store.Task, error) {
	team, err := n.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	first, ok := firstEnabled(team)
	if !ok {
		return nil, errNoEnabledRoles(projectID)
	}

	if name == "" {
		return nil, invalid("a task needs a name; the name follows the card through the whole pipeline")
	}

	// The card and the message that starts it are written together. Created
	// separately, a failure in between left a task on the board that nothing
	// had queued and nothing would ever move — a card that looks like work and
	// is not.
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning task: %w", err)
	}
	defer tx.Rollback()

	now := n.now()
	task := &store.Task{
		ID:        store.NewID(),
		ProjectID: projectID,
		Name:      name,
		Body:      body,
		Lane:      first.Name,
		State:     store.TaskQueued,
		CreatedAt: now.UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (id, project_id, name, body, lane, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		task.ID, task.ProjectID, task.Name, task.Body, task.Lane, task.State,
		task.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("creating task %q: %w", name, err)
	}

	// The opening message comes from the operator, not from a role.
	msg := &store.Message{
		ID:        store.NewID(),
		ProjectID: projectID,
		TaskID:    &task.ID,
		FromRole:  "operator",
		Kind:      store.KindNote,
		Priority:  50,
		Body:      body,
		CreatedAt: now,
	}
	if err := n.sendIn(ctx, tx, msg, sendReq{
		ProjectID: projectID,
		TaskID:    &task.ID,
		FromRole:  "operator",
		ToRoles:   []string{first.Name},
		Kind:      store.KindNote,
		Priority:  50,
		Body:      body,
		gate:      store.GateNone, // the operator does not gate their own request
	}, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing task: %w", err)
	}
	return task, nil
}

// ── sending ───────────────────────────────────────────────────────────────

// SendRequest is what a role asks for when it hands work on.
type SendRequest struct {
	TaskID   string
	Kind     string // handoff or note; defaults to handoff
	Priority int    // 0 uses 50
	Commit   string // required for a handoff
	Body     string

	// To names the recipient. Leaving it empty means "this task is finished",
	// which only the terminal role may say.
	To string

	// SourceLease is the claim this send reports on, set by the authenticated
	// boundary rather than by the agent. It is what makes a send idempotent: a
	// reply that never arrived is retried, and the retry has to be absorbed
	// rather than producing a second hand-off, a second claim and a second turn
	// of model work on the same result.
	//
	// Empty for sends that are not a role reporting on claimed work — an
	// opening message has no lease behind it.
	SourceLease string
}

// opKey identifies one send inside one lease.
//
// The operation, not the moment: recipient, kind, commit and note. A genuine
// second note to the same role still differs in its body and still sends; only
// a repeat of the identical call is absorbed.
func opKey(req SendRequest) string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{req.To, req.Kind, req.Commit, req.Body}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Send routes work from one role onward.
//
// A handoff from a gated role is held rather than queued: the recipient never
// sees it until a human approves — as opposed to moving the card, telling the
// sender it queued successfully, and leaving it somewhere nothing points at.
func (n *Nydus) Send(ctx context.Context, projectID, fromRole string, req SendRequest) (*store.Message, error) {
	team, err := n.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sender, ok := roleNamed(team, fromRole)
	if !ok {
		return nil, invalid("role %q is not on this project's team", fromRole)
	}

	// A repeat of a send that already happened returns what it produced the
	// first time. Checked before anything else because completion integrates
	// before it records: without this a lost response followed by a retry runs
	// the merge or opens the pull request a second time.
	if req.SourceLease != "" {
		if prior, err := n.priorSend(ctx, req.SourceLease, opKey(req)); err != nil {
			return nil, err
		} else if prior != nil {
			return prior, nil
		}
	}

	kind := req.Kind
	if kind == "" {
		kind = store.KindHandoff
	}
	if kind == store.KindHandoff && req.Commit == "" {
		return nil, invalid("a handoff must carry a commit; it points at committed state rather than a diff")
	}
	// And it must say what happened. A commit sha tells the next role where to
	// look, not what was decided, what was left out, or what to check — and it
	// tells the operator nothing at all. Without this a finished task reads as
	// the word "done" and nothing else, which is what the board showed before
	// this was required.
	if kind == store.KindHandoff && strings.TrimSpace(req.Body) == "" {
		return nil, invalid(
			"a handoff needs --body: what you did, what you decided, and anything the next role should check")
	}

	// Pin the commit to an absolute sha, in the sender's own tree, before it
	// goes anywhere.
	//
	// Agents write `--commit HEAD`, which is the natural thing to write and
	// which we ask them for. But HEAD means "the tip of whichever tree is
	// reading it", so the string that means "my new commit" in the coder's
	// worktree means "main's tip" once the orchestrator applies it at the
	// project root — and `merge --ff-only HEAD` there is a no-op that reports
	// success. That is not hypothetical: the first completed task merged
	// nothing, marked itself done, and left the base branch on its initial
	// commit while the board showed green.
	//
	// A ref is only meaningful in the tree that resolves it, so it is resolved
	// once, here, at the boundary it is about to cross.
	if req.Commit != "" {
		sha, err := n.resolveCommit(ctx, projectID, fromRole, req.Commit)
		if err != nil {
			return nil, err
		}
		req.Commit = sha
	}

	// An empty recipient means completion, and completion is the terminal
	// role's word alone. Anyone else omitting --to has made a mistake worth
	// reporting rather than guessing about.
	if req.To == "" {
		if !sender.Terminal {
			return nil, invalid("role %q must name a recipient; only the terminal role finishes a task", fromRole)
		}
		// A gated terminal role waits for a human before its work reaches the
		// base branch. The gate used to be applied only further down, to
		// routed hand-offs, and completion returned before reaching it — so a
		// reviewer set to require approval merged without asking. The setting
		// did the opposite of what it said, to the one action that modifies
		// the repository.
		if sender.Gate == store.GateApproval {
			return n.holdCompletion(ctx, projectID, sender, req)
		}
		return n.complete(ctx, projectID, sender, req)
	}

	recipient, ok := roleNamed(team, req.To)
	if !ok {
		return nil, invalid("role %q is not on this project's team", req.To)
	}

	// A handoff to an earlier position is rework — a reviewer returning work to
	// the coder that produced it, most often. This is allowed and expected;
	// forbidding it would leave a reviewer unable to act on what it found. It
	// is counted because backward edges make cycles possible, and a pipeline
	// that bills per lap should not be able to loop quietly.
	backward := kind == store.KindHandoff && recipient.Position < sender.Position

	priority := req.Priority
	if priority == 0 {
		priority = 50
	}
	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	return n.send(ctx, sendReq{
		ProjectID:   projectID,
		TaskID:      taskID,
		FromRole:    fromRole,
		ToRoles:     []string{req.To},
		Kind:        kind,
		Priority:    priority,
		Commit:      req.Commit,
		Body:        req.Body,
		gate:        sender.Gate,
		rework:      backward,
		sourceLease: req.SourceLease,
		opKey:       opKey(req),
	})
}

type sendReq struct {
	ProjectID string
	TaskID    *string
	FromRole  string
	ToRoles   []string
	Kind      string
	Priority  int
	Commit    string
	Body      string
	terminal  bool
	gate      string
	rework    bool

	// sourceLease and opKey make one send happen once. Both empty for a
	// message no role claimed anything to produce.
	sourceLease string
	opKey       string
}

func (n *Nydus) send(ctx context.Context, req sendReq) (*store.Message, error) {
	now := n.now()
	msg := &store.Message{
		ID:        store.NewID(),
		ProjectID: req.ProjectID,
		TaskID:    req.TaskID,
		FromRole:  req.FromRole,
		Kind:      req.Kind,
		Priority:  req.Priority,
		Body:      req.Body,
		Terminal:  req.terminal,
		CreatedAt: now,
	}
	if req.Commit != "" {
		c := req.Commit
		msg.CommitSHA = &c
	}

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning send: %w", err)
	}
	defer tx.Rollback()

	// Before the message exists, not just before the card moves. A gated role's
	// handoff writes an approval and leaves the card where it is, so guarding
	// only the move let a stopped card collect a pending approval that would
	// have walked it onward the moment anybody pressed the button.
	if req.TaskID != nil {
		if err := ensureOpen(ctx, tx, req.ProjectID, *req.TaskID); err != nil {
			return nil, err
		}
	}
	if err := n.sendIn(ctx, tx, msg, req, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing send: %w", err)
	}
	return msg, nil
}

// sendIn writes a message and its routes inside a caller's transaction.
//
// Separate from send so a caller that has more to write can do it atomically —
// NewTask creates the card and its opening message together, and a failure
// after the card existed left a task on the board that nothing had queued and
// nothing would ever move.
func (n *Nydus) sendIn(ctx context.Context, tx *sql.Tx, msg *store.Message, req sendReq, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, priority,
		   commit_sha, body, terminal, created_at, source_lease_id, op_key)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.ProjectID, msg.TaskID, msg.FromRole, msg.Kind, msg.Priority,
		msg.CommitSHA, msg.Body, msg.Terminal, now.Format(time.RFC3339Nano),
		nullable(req.sourceLease), nullable(req.opKey)); err != nil {
		return fmt.Errorf("recording message: %w", err)
	}

	// A gated handoff is held; everything else is immediately claimable. The
	// message and its routes are written together, so there is no window in
	// which a message exists with nobody to deliver it to.
	state := store.RouteQueued
	if req.gate == store.GateApproval && req.Kind == store.KindHandoff {
		state = store.RouteHeld
	}
	for _, to := range req.ToRoles {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO routes (message_id, to_role, state, enqueued_at) VALUES (?,?,?,?)`,
			msg.ID, to, state, now.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("routing to %s: %w", to, err)
		}
	}

	if state == store.RouteHeld {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO approvals (id, project_id, message_id, state, created_at)
			 VALUES (?,?,?,?,?)`,
			store.NewID(), req.ProjectID, msg.ID, store.ApprovalPending,
			now.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("opening approval: %w", err)
		}
	} else if req.TaskID != nil {
		// The card follows the work. State stays "queued" until the recipient
		// claims it, so the board distinguishes waiting from being worked on.
		//
		// The rework counter moves with it, in the same transaction: a lap that
		// was routed but not counted is exactly the invisible loop this exists
		// to prevent.
		//
		// Scoped by project and checked for one affected row. The id reaching
		// here has already been resolved inside the sender's project, but this
		// is the statement that moves work between roles, and "it updated
		// nothing" and "it updated somebody else's card" should both be errors
		// rather than a silent success the board later disagrees with.
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET lane = ?, state = ?, rework_count = rework_count + ?
			  WHERE id = ? AND project_id = ?`,
			req.ToRoles[0], store.TaskQueued, boolToInt(req.rework), *req.TaskID, req.ProjectID)
		if err != nil {
			return fmt.Errorf("moving card: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return invalid("task %s is not this project's", *req.TaskID)
		}
	}
	return nil
}

// complete finishes a task: the overmind merges the terminal commit into the
// base branch and the card lands in Done.
func (n *Nydus) complete(ctx context.Context, projectID string, sender store.ResolvedRole, req SendRequest) (*store.Message, error) {
	if req.TaskID == "" {
		return nil, invalid("completing a task requires its id")
	}
	// Completion is the one hand-off that changes the project's own branch, so
	// it is the last place to accept a missing commit. The guard used to be
	// `req.Commit != ""` around the merge alone, which turned "no commit" into
	// "integrate nothing, report done".
	if req.Commit == "" {
		return nil, invalid("finishing a task requires the commit to integrate")
	}
	// Before the body is rewritten below. The key has to describe the call that
	// was made, or a retry of the same call would hash differently and be
	// treated as a new one.
	key := opKey(req)

	task, err := n.db.GetTask(ctx, req.TaskID)
	if err != nil {
		return nil, err
	}

	// Integrate before recording completion. If it fails the card stays where
	// it is and the error reaches the operator, rather than the board claiming
	// success over a branch that never moved.
	//
	// How to integrate is the project's decision, not the role's: only the
	// terminal role gets here, and which role that is changes when the team
	// does.
	// What happened to the work, recorded with the card rather than left to be
	// reconstructed later from the project's setting as it stands then, or from
	// the sentence appended to the note below.
	outcome, outcomeRef := "", ""
	var published string
	if n.integrator != nil {
		project, err := n.db.GetProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		switch project.Integration {
		case store.IntegrateBranch:
			// Nothing to do. The work is committed on the role's branch and
			// landing it is someone else's decision.
			outcome, outcomeRef = store.OutcomeBranch, req.Commit

		case store.IntegratePR:
			// The handoff note becomes the description — it is already an
			// account of what was done and what was checked, written for
			// whoever reads next.
			url, err := n.integrator.Publish(ctx, project.Path, project.BaseBranch,
				req.Commit, task.Name, req.Body, project.PRDraft)
			if err != nil {
				return nil, fmt.Errorf("opening a pull request for %s: %w", task.Name, err)
			}
			published = url
			outcome, outcomeRef = store.OutcomePR, url

		default:
			if err := n.integrator.Merge(ctx, project.Path, project.BaseBranch, req.Commit); err != nil {
				return nil, fmt.Errorf("merging %s into %s: %w", req.Commit, project.BaseBranch, err)
			}
			outcome, outcomeRef = store.OutcomeMerged, req.Commit
		}
	}

	// The link belongs with the account of the task, so the detail view shows
	// where the work went rather than only that it finished.
	if published != "" {
		req.Body = strings.TrimSpace(req.Body) + "\n\nPull request: " + published
	}

	now := n.now()
	msg := &store.Message{
		ID: store.NewID(), ProjectID: projectID, TaskID: &task.ID,
		FromRole: sender.Name, Kind: store.KindHandoff, Priority: 50,
		Body: req.Body, Terminal: true, CreatedAt: now,
	}
	if req.Commit != "" {
		c := req.Commit
		msg.CommitSHA = &c
	}

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning completion: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, priority,
		   commit_sha, body, terminal, created_at, source_lease_id, op_key)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.ProjectID, msg.TaskID, msg.FromRole, msg.Kind, msg.Priority,
		msg.CommitSHA, msg.Body, true, now.Format(time.RFC3339Nano),
		nullable(req.SourceLease), nullable(key)); err != nil {
		return nil, fmt.Errorf("recording completion: %w", err)
	}
	if err := ensureOpen(ctx, tx, projectID, task.ID); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET lane = ?, state = ?, completed_at = ?, outcome = ?, outcome_ref = ?
		  WHERE id = ? AND project_id = ?`,
		store.LaneDone, store.TaskDone, now.Format(time.RFC3339Nano), outcome, outcomeRef,
		task.ID, projectID)
	if err != nil {
		return nil, fmt.Errorf("closing card: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, invalid("task %s is not this project's", task.ID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing completion: %w", err)
	}
	if n.onTaskDone != nil {
		var sha string
		if msg.CommitSHA != nil {
			sha = *msg.CommitSHA
		}
		n.onTaskDone(ctx, projectID, task.ID, sha)
	}
	return msg, nil
}

// ── claiming ──────────────────────────────────────────────────────────────

// Claim takes work for a role, returning nil when the queue is empty.
//
// Selection and the state change happen in one transaction, so two concurrent
// claims can never take the same message. Listing a directory and then moving
// a file lets two runs pick the same item, and the loser throws — or, in batch
// mode, splits the queue into two directories that every later call then
// refuses to touch, with no recovery path.
func (n *Nydus) Claim(ctx context.Context, projectID, role string) (*store.Lease, error) {
	team, err := n.db.ResolveTeam(ctx, projectID)
	if err != nil {
		return nil, err
	}
	r, ok := roleNamed(team, role)
	if !ok {
		return nil, invalid("role %q is not on this project's team", role)
	}

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning claim: %w", err)
	}
	defer tx.Rollback()

	// A role holding an open lease is resuming, not claiming again: handing it
	// a second unit while the first is unacknowledged is how work gets lost.
	if open, err := openLease(ctx, tx, projectID, role); err != nil {
		return nil, err
	} else if open != nil {
		items, err := leaseItems(ctx, tx, open.ID)
		if err != nil {
			return nil, err
		}
		open.Items = items
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		n.deliverCommits(ctx, projectID, open)
		return open, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT `+prefixedMessageCols+`, r.enqueued_at
		 FROM routes r JOIN messages m ON m.id = r.message_id
		 WHERE r.to_role = ? AND r.state = ? AND m.project_id = ?
		 ORDER BY m.priority ASC, m.created_at ASC`,
		role, store.RouteQueued, projectID)
	if err != nil {
		return nil, fmt.Errorf("reading queue: %w", err)
	}

	type candidate struct {
		msg        store.Message
		enqueuedAt time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var enq sql.NullString
		m, err := scanMessageRow(rows, &enq)
		if err != nil {
			rows.Close()
			return nil, err
		}
		at, err := parseNull(enq)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate{msg: *m, enqueuedAt: at})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil // no work; not an error
	}

	// Task mode takes one. Batch mode takes the head plus everything sharing
	// its priority, bounded by both max_items and max_age — an unbounded batch
	// starves a higher-priority item that arrives a millisecond late.
	take := []store.Message{candidates[0].msg}
	if r.Receive == store.ReceiveBatch {
		head := candidates[0]
		maxAge := time.Duration(r.BatchMaxAgeSec) * time.Second
		for _, c := range candidates[1:] {
			if len(take) >= r.BatchMaxItems {
				break
			}
			if c.msg.Priority != head.msg.Priority {
				break
			}
			if c.enqueuedAt.Sub(head.enqueuedAt) > maxAge {
				break
			}
			take = append(take, c.msg)
		}
	}

	now := n.now()
	lease := &store.Lease{
		ID: store.NewID(), ProjectID: projectID, Role: role,
		GrantedAt: now, ExpiresAt: now.Add(n.leaseFor), Items: take,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO leases (id, project_id, role, granted_at, expires_at) VALUES (?,?,?,?,?)`,
		lease.ID, projectID, role, now.Format(time.RFC3339Nano),
		lease.ExpiresAt.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("granting lease: %w", err)
	}

	for _, m := range take {
		// The guard on state is what makes the claim atomic: a concurrent
		// claimer that got here first leaves this update affecting zero rows.
		res, err := tx.ExecContext(ctx,
			`UPDATE routes SET state = ?, delivered_at = ?
			 WHERE message_id = ? AND to_role = ? AND state = ?`,
			store.RouteClaimed, now.Format(time.RFC3339Nano), m.ID, role, store.RouteQueued)
		if err != nil {
			return nil, fmt.Errorf("claiming message: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if affected == 0 {
			return nil, errRaced
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO lease_items (lease_id, message_id, to_role) VALUES (?,?,?)`,
			lease.ID, m.ID, role); err != nil {
			return nil, fmt.Errorf("recording lease item: %w", err)
		}

		if m.TaskID != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE tasks SET state = ?,
				   first_claimed_at = COALESCE(first_claimed_at, ?)
				 WHERE id = ? AND state NOT IN (?, ?)`,
				store.TaskWorking, now.Format(time.RFC3339Nano), *m.TaskID,
				store.TaskDone, store.TaskRejected); err != nil {
				return nil, fmt.Errorf("marking task working: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing claim: %w", err)
	}
	n.deliverCommits(ctx, projectID, lease)
	return lease, nil
}

// deliverCommits merges each claimed commit into the role's worktree, so a
// role opens its tree and finds the work it was handed already in it.
//
// This runs after the transaction commits, never inside it: a git subprocess
// holding the single write lock would block every other writer for as long as
// the merge took.
//
// Failure is not an error here. A merge can conflict, and a conflict is a
// normal outcome of parallel work, not a reason to refuse to hand over the
// task — the agent is told merged=false and resolves it in the tree where it
// happened. What must never happen is claiming the merge succeeded when it
// did not; that was the original bug, and it cost a reviewer two rounds of
// discovering its worktree did not contain the code it was reviewing.
// resolveCommit expands a commit-ish to a full sha using the sending role's
// worktree. With no integrator or no worktree — tests, mostly — the value is
// passed through, since there is nothing to resolve it against.
func (n *Nydus) resolveCommit(ctx context.Context, projectID, role, ref string) (string, error) {
	if n.integrator == nil {
		return ref, nil
	}
	project, err := n.db.GetProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	worktree := hatchery.New(project.Path).Path(role)
	if _, err := os.Stat(worktree); err != nil {
		return ref, nil
	}
	sha, err := n.integrator.Resolve(ctx, worktree, ref)
	if err != nil {
		return "", invalid("%q does not name a commit in %s's worktree: %v", ref, role, err)
	}
	return sha, nil
}

func (n *Nydus) deliverCommits(ctx context.Context, projectID string, lease *store.Lease) {
	lease.Merged = make(map[string]bool, len(lease.Items))

	var worktree string
	for _, m := range lease.Items {
		if m.CommitSHA == nil || *m.CommitSHA == "" {
			continue
		}
		if worktree == "" {
			project, err := n.db.GetProject(ctx, projectID)
			if err != nil {
				return
			}
			worktree = hatchery.New(project.Path).Path(lease.Role)
			if _, err := os.Stat(worktree); err != nil {
				return // no worktree for this role; nothing to merge into
			}
		}
		if err := n.integrator.MergeInto(ctx, worktree, *m.CommitSHA); err != nil {
			// Left false. The envelope carries the truth either way.
			continue
		}
		lease.Merged[m.ID] = true
	}
}

// errRaced means another claimer took the work between selection and update.
// The caller retries; the transaction rolled back, so nothing half-applied.
var errRaced = errors.New("nydus: another claimer took this work")

// ── acknowledging ─────────────────────────────────────────────────────────

// Ack closes a lease. It is idempotent: acknowledging twice is not an error,
// because an agent that crashed after acking and retried on restart should not
// be told it did something wrong.
//
// It takes the caller's word for the lease id. Anything holding an agent's
// identity must use AckOwned instead.
func (n *Nydus) Ack(ctx context.Context, leaseID string) error {
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning ack: %w", err)
	}
	defer tx.Rollback()

	var (
		projectID string
		grantedAt string
		acked     sql.NullString
		expired   sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT project_id, granted_at, acked_at, expired_at FROM leases WHERE id = ?`, leaseID).
		Scan(&projectID, &grantedAt, &acked, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lease %s: %w", leaseID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading lease: %w", err)
	}
	if acked.Valid {
		return nil // already acknowledged
	}
	if expired.Valid {
		return invalid("lease %s expired and its work was returned to the queue", leaseID)
	}

	now := n.now()
	granted, err := time.Parse(time.RFC3339Nano, grantedAt)
	if err != nil {
		return fmt.Errorf("lease %s has an unreadable granted_at: %w", leaseID, err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE leases SET acked_at = ? WHERE id = ?`,
		now.Format(time.RFC3339Nano), leaseID); err != nil {
		return fmt.Errorf("closing lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE routes SET state = ?
		 WHERE (message_id, to_role) IN (SELECT message_id, to_role FROM lease_items WHERE lease_id = ?)`,
		store.RouteDone, leaseID); err != nil {
		return fmt.Errorf("closing routes: %w", err)
	}

	// Worked time accrues per lease, so a task's active_ms stays separable from
	// its wall time and a blocked pipeline is visible as the gap between them.
	activeMS := now.Sub(granted).Milliseconds()
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET active_ms = active_ms + ?
		 WHERE id IN (
		   SELECT DISTINCT m.task_id FROM lease_items li
		   JOIN messages m ON m.id = li.message_id
		   WHERE li.lease_id = ? AND m.task_id IS NOT NULL)`,
		activeMS, leaseID); err != nil {
		return fmt.Errorf("recording worked time: %w", err)
	}

	return tx.Commit()
}

// ReclaimOrphanedLeases returns every open lease's work to the queue, and is
// called once when the daemon starts.
//
// Agents are child processes of the daemon, so no lease that predates this
// process can still have a holder. Waiting for the deadline instead means work
// in flight at a restart sits untouched for up to the full lease period: the
// route stays `claimed` rather than `queued`, so nothing nudges anyone, and the
// board shows a card being worked by an agent that no longer exists.
//
// Observed after a routine restart — a card reading "working" with a lease
// twenty minutes from expiry and two live agents that had been handed nothing.
func (n *Nydus) ReclaimOrphanedLeases(ctx context.Context) (int, error) {
	return n.expire(ctx, time.Time{}, "")
}

// ReclaimLeases returns one project's in-flight work to the queue.
//
// Called when a swarm is stopped. The daemon has just killed those agents, so
// it knows the leases cannot have holders — leaving them to lapse on their own
// means the work sits `claimed` for up to the full lease period while the
// replacement agents stand idle beside it. Observed: a card reading "working"
// for twenty minutes after a stop and start, with three ready agents and
// nothing handed to any of them.
func (n *Nydus) ReclaimLeases(ctx context.Context, projectID string) (int, error) {
	return n.expire(ctx, time.Time{}, projectID)
}

// ExpireLeases returns unacknowledged work to the queue and reports how many
// leases lapsed. Without this a crashed agent takes its work with it, and the
// pipeline stalls with nothing to notice that it has.
func (n *Nydus) ExpireLeases(ctx context.Context) (int, error) {
	return n.expire(ctx, n.now(), "")
}

// ensureOpen refuses to move a card that is already finished.
//
// The two statements that move a card set its state unconditionally, so a
// handoff arriving after the card was stopped simply set it back to "queued"
// and routed it onward — the stop had stopped nothing, and the board showed a
// card walking down a pipeline nobody had asked it to re-enter. Observed:
// stopping a card while its role was mid-turn, then watching that role finish
// and hand the work on four minutes later.
//
// A role can be mid-turn when a person stops its card, so this is expected
// rather than exceptional. It is invalid() so the agent is told plainly why its
// handoff was refused, and by name, rather than reading a 500.
func ensureOpen(ctx context.Context, tx *sql.Tx, projectID, taskID string) error {
	var (
		state   string
		stopped sql.NullString
	)
	err := tx.QueryRowContext(ctx,
		`SELECT state, stopped_at FROM tasks WHERE id = ? AND project_id = ?`,
		taskID, projectID).Scan(&state, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("task %s is not this project's", taskID)
	}
	if err != nil {
		return fmt.Errorf("reading the card: %w", err)
	}
	switch {
	case state == store.TaskDone:
		return invalid("that card is already finished; nothing more is routed for it")
	case state == store.TaskRejected && stopped.Valid:
		return invalid("that card was stopped; nothing more is routed for it")
	case state == store.TaskRejected:
		return invalid("that card was rejected; nothing more is routed for it")
	}
	return nil
}

// expire requeues open leases. A zero deadline means every one of them,
// regardless of when it lapses; otherwise only those already past it.
//
// One body for both callers on purpose: requeueing is the delicate part, and
// two copies of it would be two chances to get the route state wrong.
func (n *Nydus) expire(ctx context.Context, deadline time.Time, projectID string) (int, error) {
	now := n.now()

	where := `acked_at IS NULL AND expired_at IS NULL`
	args := []any{}
	if !deadline.IsZero() {
		where += ` AND expires_at < ?`
		args = append(args, deadline.Format(time.RFC3339Nano))
	}
	if projectID != "" {
		where += ` AND project_id = ?`
		args = append(args, projectID)
	}

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning expiry: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM leases WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("finding lapsed leases: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET expired_at = ? WHERE id = ?`,
			now.Format(time.RFC3339Nano), id); err != nil {
			return 0, fmt.Errorf("expiring lease: %w", err)
		}
		// Back to queued, not lost. The message is unchanged, so whoever picks
		// it up next sees exactly what the previous holder saw.
		if _, err := tx.ExecContext(ctx,
			`UPDATE routes SET state = ?, delivered_at = NULL
			 WHERE state = ?
			   AND (message_id, to_role) IN (SELECT message_id, to_role FROM lease_items WHERE lease_id = ?)`,
			store.RouteQueued, store.RouteClaimed, id); err != nil {
			return 0, fmt.Errorf("requeueing work: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?
			 WHERE state = ? AND id IN (
			   SELECT DISTINCT m.task_id FROM lease_items li
			   JOIN messages m ON m.id = li.message_id
			   WHERE li.lease_id = ? AND m.task_id IS NOT NULL)`,
			store.TaskQueued, store.TaskWorking, id); err != nil {
			return 0, fmt.Errorf("resetting task state: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing expiry: %w", err)
	}
	return len(ids), nil
}

// ── approvals ─────────────────────────────────────────────────────────────

// Approve releases a held handoff to its recipient.
func (n *Nydus) Approve(ctx context.Context, approvalID string) error {
	return n.decide(ctx, approvalID, store.ApprovalApproved, "")
}

// Reject returns a held handoff to its sender with a note, and nothing
// downstream ever saw it.
func (n *Nydus) Reject(ctx context.Context, approvalID, note string) error {
	return n.decide(ctx, approvalID, store.ApprovalRejected, note)
}

func (n *Nydus) decide(ctx context.Context, approvalID, decision, note string) error {
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning decision: %w", err)
	}
	defer tx.Rollback()

	var (
		messageID string
		state     string
		taskID    sql.NullString
		fromRole  string
		terminal  int
		commit    sql.NullString
		body      string
		projectID string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT a.message_id, a.state, m.task_id, m.from_role,
		        m.terminal, m.commit_sha, m.body, a.project_id
		 FROM approvals a JOIN messages m ON m.id = a.message_id
		 WHERE a.id = ?`, approvalID).Scan(&messageID, &state, &taskID, &fromRole,
		&terminal, &commit, &body, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("approval %s: %w", approvalID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading approval: %w", err)
	}
	if state != store.ApprovalPending {
		return invalid("approval %s was already %s", approvalID, state)
	}

	// A card does not merge with a question still open on it, which is the
	// whole reason a review remark is a row with a state rather than a note in
	// a body. Only approving is refused: rejecting *is* the answer to those
	// threads, and sending the card back with them unsettled is the point.
	if decision == store.ApprovalApproved && taskID.Valid {
		open, err := n.db.OpenReviewThreads(ctx, taskID.String)
		if err != nil {
			return err
		}
		if open > 0 {
			return invalid("%d review thread(s) are still open on this card; settle them or reject", open)
		}
	}

	// Approving a completion lands it, and that happens before the decision is
	// recorded — the same order complete() uses. If the merge fails nothing has
	// been written, the approval is still pending, and it can be approved again
	// once the cause is fixed. Recording first would leave a decision standing
	// over a branch that never moved.
	//
	// The transaction is not holding anything yet that this needs, and a git
	// subprocess must never run while it holds the single write lock.
	var landed, outcome, outcomeRef string
	if decision == store.ApprovalApproved && terminal != 0 && taskID.Valid {
		// Claim it before doing anything irreversible.
		//
		// The check above ran in a transaction that is about to be released,
		// because a git subprocess must never hold the single writer. Without a
		// claim, two decisions could both read pending, both merge, and the
		// later one overwrite the earlier — an approve racing a reject recorded
		// "rejected" over a branch that had already landed.
		res, err := tx.ExecContext(ctx,
			`UPDATE approvals SET state = ? WHERE id = ? AND state = ?`,
			store.ApprovalIntegrating, approvalID, store.ApprovalPending)
		if err != nil {
			return fmt.Errorf("claiming approval: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return invalid("approval %s is already being decided", approvalID)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("claiming approval: %w", err)
		}

		if landed, outcome, outcomeRef, err = n.landApproved(ctx, projectID, taskID.String, commit.String, body); err != nil {
			// Hand it back, or a failed merge would leave the card stuck
			// mid-decision with no way to retry.
			if _, rerr := n.db.SQL().ExecContext(ctx,
				`UPDATE approvals SET state = ? WHERE id = ? AND state = ?`,
				store.ApprovalPending, approvalID, store.ApprovalIntegrating); rerr != nil {
				return fmt.Errorf("%w (and releasing the claim failed: %v)", err, rerr)
			}
			return err
		}
		if tx, err = n.db.SQL().BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("reopening after landing: %w", err)
		}
		defer tx.Rollback()
	}
	_ = landed

	at := n.now()
	now := at.Format(time.RFC3339Nano)
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	// Guarded on the state this decision left behind: pending for a routed
	// handoff, integrating for one that just landed. A decision that lost the
	// race writes nothing.
	expect := store.ApprovalPending
	if landed != "" || (decision == store.ApprovalApproved && terminal != 0 && taskID.Valid) {
		expect = store.ApprovalIntegrating
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET state = ?, note = ?, decided_at = ? WHERE id = ? AND state = ?`,
		decision, notePtr, now, approvalID, expect)
	if err != nil {
		return fmt.Errorf("recording decision: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return invalid("approval %s was decided by someone else", approvalID)
	}

	if decision == store.ApprovalApproved && terminal != 0 {
		// A completion has no routes to release; approving it finishes the task.
		if taskID.Valid {
			if _, err := tx.ExecContext(ctx,
				`UPDATE tasks SET lane = ?, state = ?, completed_at = ?, outcome = ?, outcome_ref = ?
				  WHERE id = ?`,
				store.LaneDone, store.TaskDone, now, outcome, outcomeRef, taskID.String); err != nil {
				return fmt.Errorf("closing card: %w", err)
			}
		}
	} else if decision == store.ApprovalApproved {
		if _, err := tx.ExecContext(ctx,
			`UPDATE routes SET state = ?, enqueued_at = ? WHERE message_id = ? AND state = ?`,
			store.RouteQueued, now, messageID, store.RouteHeld); err != nil {
			return fmt.Errorf("releasing handoff: %w", err)
		}
		// Only now does the card move: an approval gate that moved the card on
		// send would show work sitting with a role that cannot see it.
		if taskID.Valid {
			var toRole string
			if err := tx.QueryRowContext(ctx,
				`SELECT to_role FROM routes WHERE message_id = ? LIMIT 1`, messageID).Scan(&toRole); err != nil {
				return fmt.Errorf("finding recipient: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET lane = ?, state = ? WHERE id = ?`,
				toRole, store.TaskQueued, taskID.String); err != nil {
				return fmt.Errorf("moving card: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE routes SET state = ? WHERE message_id = ? AND state = ?`,
			store.RouteRejected, messageID, store.RouteHeld); err != nil {
			return fmt.Errorf("rejecting handoff: %w", err)
		}
		// The card goes back to whoever wrote it, with the reason attached and
		// claimable.
		//
		// Moving the lane was not enough and nothing said so. A role is given
		// work by a message with a route; a card whose lane changed and whose
		// only queued route was the one just rejected left the author with
		// nothing to claim, so a rejected card sat in their column and no agent
		// ever picked it up again. The reason lived on the approval row, which
		// no agent reads.
		if taskID.Valid {
			feedback, err := rejectionNote(ctx, tx, note, taskID.String)
			if err != nil {
				return err
			}
			msg := &store.Message{
				ID: store.NewID(), ProjectID: projectID, TaskID: &taskID.String,
				FromRole: store.OperatorRole, Kind: store.KindNote, Priority: 50,
				Body: feedback, CreatedAt: at,
			}
			// sendIn moves the card as it routes, which is the same lane change
			// this used to do by hand.
			if err := n.sendIn(ctx, tx, msg, sendReq{
				ProjectID: projectID,
				TaskID:    &taskID.String,
				FromRole:  store.OperatorRole,
				ToRoles:   []string{fromRole},
				Kind:      store.KindNote,
				Priority:  50,
				Body:      feedback,
				gate:      store.GateNone, // a rejection is not itself gated
			}, at); err != nil {
				return fmt.Errorf("returning the card to %s: %w", fromRole, err)
			}
		}
	}

	// A gated completion lands here rather than in complete(), and this hook
	// was only wired to the ungated one: on any project with an approval gate
	// at the end -- which is the arrangement the gate exists for -- nothing
	// that runs when a card lands ran at all. The disk sweep was the first
	// casualty and nobody noticed, because a swarm that never reclaims looks
	// exactly like a swarm that produces a lot of build output.
	finished := decision == store.ApprovalApproved && terminal != 0 && taskID.Valid

	if err := tx.Commit(); err != nil {
		return err
	}
	if finished && n.onTaskDone != nil {
		n.onTaskDone(ctx, projectID, taskID.String, commit.String)
	}
	return nil
}

// rejectionNote is what the author is told: the reason, and every remark still
// open on the card.
//
// The remarks are the review. Left in the database they were visible to the
// person who wrote them and to nobody else, so rejecting with an empty reason
// -- which is what happens when the reasons are already written on the lines
// they belong to -- sent the work back with no feedback at all.
//
// Anchors included, because "this needs a test" means nothing without the file
// and the line it was said about.
func rejectionNote(ctx context.Context, tx *sql.Tx, note, taskID string) (string, error) {
	var b strings.Builder
	if strings.TrimSpace(note) != "" {
		b.WriteString(strings.TrimSpace(note))
	} else {
		b.WriteString("This was rejected at the approval gate.")
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.file, t.line, c.author, c.body
		   FROM review_threads t JOIN review_comments c ON c.thread_id = t.id
		  WHERE t.task_id = ? AND t.state = ? AND t.kind = ?
		  ORDER BY t.file, t.line, t.id, c.created_at, c.id`,
		taskID, store.ThreadOpen, store.ThreadRemark)
	if err != nil {
		return "", fmt.Errorf("reading the review: %w", err)
	}
	defer rows.Close()

	lastThread := ""
	for rows.Next() {
		var id, file, author, text string
		var line int
		if err := rows.Scan(&id, &file, &line, &author, &text); err != nil {
			return "", fmt.Errorf("reading the review: %w", err)
		}
		if id != lastThread {
			if lastThread == "" {
				b.WriteString("\n\nUnsettled remarks from the review:")
			}
			b.WriteString("\n\n")
			switch {
			case file != "" && line > 0:
				fmt.Fprintf(&b, "%s:%d", file, line)
			case file != "":
				b.WriteString(file)
			default:
				b.WriteString("on this change")
			}
			lastThread = id
		}
		fmt.Fprintf(&b, "\n  %s: %s", author, text)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("reading the review: %w", err)
	}
	return b.String(), nil
}

// ── ownership and idempotency ─────────────────────────────────────────────

// nullable turns an empty string into a SQL NULL.
//
// It matters for the idempotency index: NULLs are distinct in SQLite, so every
// message with no source lease is exempt, while an empty string would collide
// with every other empty string.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// priorSend returns the message an identical earlier send produced, or nil.
func (n *Nydus) priorSend(ctx context.Context, leaseID, key string) (*store.Message, error) {
	rows, err := n.db.Read().QueryContext(ctx,
		`SELECT `+prefixedMessageCols+`, m.op_key FROM messages m
		  WHERE m.source_lease_id = ? AND m.op_key = ? LIMIT 1`, leaseID, key)
	if err != nil {
		return nil, fmt.Errorf("checking for a repeated send: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var stored sql.NullString
	return scanMessageRow(rows, &stored)
}

// LeaseHolding returns the lease under which role holds task, if it holds it.
//
// This is what binds an action to a claim. Membership of the project is not
// enough: a token minted for a terminal role could otherwise acknowledge any
// lease id and complete any task in the project, including one it had never
// been handed and one another role was working at that moment.
//
// An acknowledged lease still counts — an agent that runs `zerg done` before
// `zerg send` is doing them in an unusual order, not doing something it may
// not do. An expired one does not: its work went back to the queue and may
// already be somebody else's.
func (n *Nydus) LeaseHolding(ctx context.Context, projectID, role, taskID string) (*store.Lease, error) {
	var (
		id      string
		expired sql.NullString
	)
	err := n.db.Read().QueryRowContext(ctx,
		`SELECT l.id, l.expired_at
		   FROM leases l
		   JOIN lease_items li ON li.lease_id = l.id
		   JOIN messages m ON m.id = li.message_id
		  WHERE l.project_id = ? AND l.role = ? AND m.task_id = ?
		  ORDER BY l.granted_at DESC LIMIT 1`, projectID, role, taskID).Scan(&id, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("role %q is not holding task %s", role, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("checking who holds this work: %w", err)
	}
	if expired.Valid {
		return nil, invalid(
			"role %q held task %s under a lease that expired; its work went back to the queue", role, taskID)
	}
	return &store.Lease{ID: id, ProjectID: projectID, Role: role}, nil
}

// AckOwned closes a lease on behalf of the role that holds it.
//
// The unauthenticated Ack below trusts its caller with a lease id. This is the
// one an authenticated caller uses: a token proves a project and a role, and a
// lease belongs to exactly one of each, so the three have to agree before
// anything is acknowledged. Without it a token minted for any role could close
// any lease in the process, returning another role's work as finished.
//
// A lease that is not this role's reports as missing rather than as forbidden.
// The caller has no business knowing it exists.
func (n *Nydus) AckOwned(ctx context.Context, projectID, role, leaseID string) error {
	var owner, ownerRole string
	err := n.db.Read().QueryRowContext(ctx,
		`SELECT project_id, role FROM leases WHERE id = ?`, leaseID).Scan(&owner, &ownerRole)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (owner != projectID || ownerRole != role)) {
		return fmt.Errorf("lease %s: %w", leaseID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading lease: %w", err)
	}
	return n.Ack(ctx, leaseID)
}

// ReconcileIntegrating settles approvals a crash left mid-decision, and is
// called once when the daemon starts.
//
// Approving a completion claims the approval, releases the write lock, runs
// git, and records the outcome in a second transaction. A daemon killed in that
// window leaves the approval reading "integrating" permanently: Attention
// excludes it, so nothing tells anyone it is there, and if the merge or the
// pull request succeeded before the crash the work has landed with a card that
// still says it is waiting.
//
// The repository is the record, so it is what settles it. Landed means finish
// recording the decision that was already carried out; not landed means hand
// the approval back to the operator as pending, exactly as a failed merge does.
func (n *Nydus) ReconcileIntegrating(ctx context.Context) (settled, released int, err error) {
	rows, err := n.db.Read().QueryContext(ctx,
		`SELECT a.id, a.project_id, m.task_id, m.commit_sha, m.body
		   FROM approvals a JOIN messages m ON m.id = a.message_id
		  WHERE a.state = ?`, store.ApprovalIntegrating)
	if err != nil {
		return 0, 0, fmt.Errorf("finding interrupted decisions: %w", err)
	}
	type stuck struct {
		id, projectID, body string
		taskID, commit      sql.NullString
	}
	var pending []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.id, &s.projectID, &s.taskID, &s.commit, &s.body); err != nil {
			rows.Close()
			return 0, 0, err
		}
		pending = append(pending, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, a := range pending {
		landed := false
		outcome, outcomeRef := "", ""
		if n.integrator != nil && a.taskID.Valid {
			project, err := n.db.GetProject(ctx, a.projectID)
			if err != nil {
				return settled, released, err
			}
			task, err := n.db.GetTask(ctx, a.taskID.String)
			if err != nil {
				return settled, released, err
			}
			var where string
			where, landed, err = n.integrator.Landed(ctx, project.Path, project.BaseBranch,
				a.commit.String, task.Name, project.Integration)
			// The mode this was integrated under is the one being asked about
			// right here, which is the only moment it can be recorded honestly,
			// and git is the only thing that still knows where the work went:
			// a gated completion's note was written before it was published.
			switch project.Integration {
			case store.IntegrateBranch:
				outcome, outcomeRef = store.OutcomeBranch, a.commit.String
			case store.IntegratePR:
				outcome, outcomeRef = store.OutcomePR, where
			default:
				outcome, outcomeRef = store.OutcomeMerged, a.commit.String
			}
			if err != nil {
				// Unanswerable is not the same as no. Leave it claimed rather
				// than releasing an approval whose merge may have happened, and
				// let the next start ask again.
				return settled, released, fmt.Errorf(
					"cannot tell whether approval %s finished integrating: %w", a.id, err)
			}
		}

		if !landed {
			if _, err := n.db.SQL().ExecContext(ctx,
				`UPDATE approvals SET state = ? WHERE id = ? AND state = ?`,
				store.ApprovalPending, a.id, store.ApprovalIntegrating); err != nil {
				return settled, released, fmt.Errorf("releasing approval %s: %w", a.id, err)
			}
			released++
			continue
		}
		if err := n.finishIntegrated(ctx, a.id, a.taskID.String, outcome, outcomeRef); err != nil {
			return settled, released, err
		}
		settled++
	}
	return settled, released, nil
}

// finishIntegrated records the approval and closes the card for work that is
// already in the base branch.
func (n *Nydus) finishIntegrated(ctx context.Context, approvalID, taskID, outcome, outcomeRef string) error {
	now := n.now().Format(time.RFC3339Nano)
	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settling approval %s: %w", approvalID, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE approvals SET state = ?, decided_at = ? WHERE id = ? AND state = ?`,
		store.ApprovalApproved, now, approvalID, store.ApprovalIntegrating)
	if err != nil {
		return fmt.Errorf("settling approval %s: %w", approvalID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil // someone else settled it
	}
	if taskID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET lane = ?, state = ?, completed_at = COALESCE(completed_at, ?),
			   outcome = ?, outcome_ref = ? WHERE id = ?`,
			store.LaneDone, store.TaskDone, now, outcome, outcomeRef, taskID); err != nil {
			return fmt.Errorf("closing card: %w", err)
		}
	}
	return tx.Commit()
}

// ── helpers ───────────────────────────────────────────────────────────────

const prefixedMessageCols = `m.id, m.project_id, m.task_id, m.from_role, m.kind,
	m.priority, m.commit_sha, m.body, m.terminal, m.created_at`

func scanMessageRow(rows *sql.Rows, extra *sql.NullString) (*store.Message, error) {
	var (
		m         store.Message
		taskID    sql.NullString
		commitSHA sql.NullString
		created   string
		terminal  int
	)
	if err := rows.Scan(&m.ID, &m.ProjectID, &taskID, &m.FromRole, &m.Kind, &m.Priority,
		&commitSHA, &m.Body, &terminal, &created, extra); err != nil {
		return nil, err
	}
	if taskID.Valid {
		m.TaskID = &taskID.String
	}
	if commitSHA.Valid {
		m.CommitSHA = &commitSHA.String
	}
	m.Terminal = terminal != 0
	var err error
	if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	return &m, nil
}

func parseNull(ns sql.NullString) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, ns.String)
}

func openLease(ctx context.Context, tx *sql.Tx, projectID, role string) (*store.Lease, error) {
	var (
		l       store.Lease
		granted string
		expires string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, project_id, role, granted_at, expires_at FROM leases
		 WHERE project_id = ? AND role = ? AND acked_at IS NULL AND expired_at IS NULL
		 ORDER BY granted_at LIMIT 1`, projectID, role).
		Scan(&l.ID, &l.ProjectID, &l.Role, &granted, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking for an open lease: %w", err)
	}
	if l.GrantedAt, err = time.Parse(time.RFC3339Nano, granted); err != nil {
		return nil, err
	}
	if l.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
		return nil, err
	}
	return &l, nil
}

func leaseItems(ctx context.Context, tx *sql.Tx, leaseID string) ([]store.Message, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+prefixedMessageCols+`, NULL
		 FROM lease_items li JOIN messages m ON m.id = li.message_id
		 WHERE li.lease_id = ? ORDER BY m.priority, m.created_at`, leaseID)
	if err != nil {
		return nil, fmt.Errorf("reading lease items: %w", err)
	}
	defer rows.Close()

	var out []store.Message
	for rows.Next() {
		var ignored sql.NullString
		m, err := scanMessageRow(rows, &ignored)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func firstEnabled(team []store.ResolvedRole) (store.ResolvedRole, bool) {
	for _, r := range team {
		if r.Enabled {
			return r, true
		}
	}
	return store.ResolvedRole{}, false
}

func roleNamed(team []store.ResolvedRole, name string) (store.ResolvedRole, bool) {
	for _, r := range team {
		if r.Name == name && r.Enabled {
			return r, true
		}
	}
	return store.ResolvedRole{}, false
}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Validation()   {}

func invalid(format string, args ...any) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

func errNoEnabledRoles(projectID string) error {
	return invalid("project %s has no enabled roles; select at least one before starting work", projectID)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// holdCompletion records a finished task that is waiting for a human.
//
// Nothing is integrated and the task is not marked done: it is finished as far
// as the roles are concerned and not yet landed, which is a real state and
// needs to be visible as one. The card stays with the role that finished it,
// so the board does not claim Done over work nobody has approved.
func (n *Nydus) holdCompletion(ctx context.Context, projectID string, sender store.ResolvedRole, req SendRequest) (*store.Message, error) {
	if req.TaskID == "" {
		return nil, invalid("completing a task requires its id")
	}
	if req.Commit == "" {
		return nil, invalid("finishing a task requires the commit to integrate")
	}
	task, err := n.db.GetTask(ctx, req.TaskID)
	if err != nil {
		return nil, err
	}

	now := n.now()
	msg := &store.Message{
		ID: store.NewID(), ProjectID: projectID, TaskID: &task.ID,
		FromRole: sender.Name, Kind: store.KindHandoff, Priority: 50,
		Body: req.Body, Terminal: true, CreatedAt: now,
	}
	c := req.Commit
	msg.CommitSHA = &c

	tx, err := n.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning held completion: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, project_id, task_id, from_role, kind, priority,
		   commit_sha, body, terminal, created_at, source_lease_id, op_key)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		msg.ID, msg.ProjectID, msg.TaskID, msg.FromRole, msg.Kind, msg.Priority,
		msg.CommitSHA, msg.Body, true, now.Format(time.RFC3339Nano),
		nullable(req.SourceLease), nullable(opKey(req))); err != nil {
		return nil, fmt.Errorf("recording held completion: %w", err)
	}
	// The approval is written in the same transaction as the message, so there
	// is no instant where a completion exists with nothing asking about it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals (id, project_id, message_id, state, created_at)
		 VALUES (?,?,?,?,?)`,
		store.NewID(), projectID, msg.ID, store.ApprovalPending,
		now.Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("recording approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing held completion: %w", err)
	}
	return msg, nil
}

// landApproved integrates a completion that a human has just approved, and
// marks the task done.
//
// Integration runs before the decision is recorded, which is the same order
// complete() uses and for the same reason: if the merge fails, nothing has been
// written, the approval is still pending, and the operator can fix the cause
// and approve again. The alternative records a decision over a branch that
// never moved.
// landApproved integrates an approved completion and says how it went: the
// outcome, and where the work ended up. Both are recorded with the card, since
// the project's integration setting is a live value and answers a different
// question tomorrow than it did at the moment a task ended.
func (n *Nydus) landApproved(ctx context.Context, projectID, taskID, commit, body string) (url, outcome, ref string, err error) {
	if n.integrator == nil {
		return "", "", "", nil
	}
	project, err := n.db.GetProject(ctx, projectID)
	if err != nil {
		return "", "", "", err
	}
	task, err := n.db.GetTask(ctx, taskID)
	if err != nil {
		return "", "", "", err
	}

	switch project.Integration {
	case store.IntegrateBranch:
		return "", store.OutcomeBranch, commit, nil
	case store.IntegratePR:
		url, err := n.integrator.Publish(ctx, project.Path, project.BaseBranch, commit, task.Name, body, project.PRDraft)
		if err != nil {
			return "", "", "", err
		}
		return url, store.OutcomePR, url, nil
	default:
		if err := n.integrator.Merge(ctx, project.Path, project.BaseBranch, commit); err != nil {
			return "", "", "", fmt.Errorf("merging %s into %s: %w", commit, project.BaseBranch, err)
		}
		return "", store.OutcomeMerged, commit, nil
	}
}
