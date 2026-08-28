package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kconfesor/zerg/internal/chat"
	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
	"github.com/kconfesor/zerg/internal/tailnet"
	"strconv"
	"time"
)

// settingsResponse is the settings form plus the facts it needs to be filled
// in sensibly: what the machine is called on the tailnet, and whether HTTPS is
// available there. Without those the TLS choice is a guess.
type settingsResponse struct {
	Config  store.Config   `json:"config"`
	Tailnet tailnet.Status `json:"tailnet"`

	// Applied is the address actually being served right now, which differs
	// from Config.Addr whenever settings have been saved but not restarted.
	Applied       string `json:"applied"`
	RestartNeeded bool   `json:"restartNeeded"`
}

// restartNeeded compares the whole listener, not just its address.
//
// Saving a TLS mode, a certificate path or the loopback door used to report
// nothing to do, because only Addr was compared — so the cockpit went on
// serving plain HTTP on an address the operator had just asked to be encrypted,
// and said everything was applied.
func (s *Server) restartNeeded(saved store.Config) bool {
	if s.applied == (store.Listener{}) {
		return false // nothing bound this process; nothing to be stale
	}
	return s.applied != saved.Listener()
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetConfig(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:        cfg,
		Tailnet:       tailnet.Probe(r.Context()),
		Applied:       s.applied.Addr,
		RestartNeeded: s.restartNeeded(cfg),
	})
}

// setSettings stores settings and says what still has to happen for them to
// take effect.
//
// Rebinding a live listener would drop every open connection, including the
// stream the caller is watching, so the address and TLS mode apply on restart
// and the response says so. Retention and cleanup apply immediately, because
// nothing is holding them.
func (s *Server) setSettings(w http.ResponseWriter, r *http.Request) {
	var cfg store.Config
	if !decode(w, r, &cfg) {
		return
	}
	saved, err := s.db.SetConfig(r.Context(), cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:        saved,
		Tailnet:       tailnet.Probe(r.Context()),
		Applied:       s.applied.Addr,
		RestartNeeded: s.restartNeeded(saved),
	})
}

// sweep reclaims disk for one project, on demand.
//
// Exposed as a button as well as a policy because "how much would this free"
// is a question best answered by doing it once and reading the number.
func (s *Server) sweep(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetConfig(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	freed, pruned, err := Sweep(r.Context(), s.db, r.PathValue("id"), cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bytesFreed":     freed,
		"branchesPruned": pruned,
	})
}

// Sweep reclaims disk for one project: ignored files out of every role's
// worktree, and optionally the role branches already merged into the base.
//
// Ignored files only, ever. A worktree is mostly regenerable bytes — 45 MB of
// build output against 256 KB of source, per role, in a real project — but an
// agent's uncommitted work lives in untracked files, and a cleanup that eats
// those is worse than a full disk.
func Sweep(ctx context.Context, db *store.DB, projectID string, cfg store.Config) (int64, []string, error) {
	project, err := db.GetProject(ctx, projectID)
	if err != nil {
		return 0, nil, err
	}
	team, err := db.ResolveTeam(ctx, projectID)
	if err != nil {
		return 0, nil, err
	}

	hat := hatchery.New(project.Path)
	var freed int64
	if cfg.CleanIgnored {
		for _, role := range team {
			n, err := hat.SweepIgnored(ctx, role.Name)
			if err != nil {
				return freed, nil, fmt.Errorf("sweeping %s: %w", role.Name, err)
			}
			freed += n
		}
	}

	var pruned []string
	if cfg.PruneMergedBranches {
		pruned, err = hat.PruneMergedBranches(ctx, project.BaseBranch)
		if err != nil {
			return freed, nil, err
		}
	}
	return freed, pruned, nil
}

type chatRequest struct {
	Message string `json:"message"`
}

// chat asks the project's chat agent a question.
//
// Returns as soon as the agent has the message, not when it has answered: the
// reply arrives as events on the stream the cockpit is already watching, so a
// long answer renders as it is written rather than after a silent wait.
func (s *Server) askChat(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "chat is not available")
		return
	}
	var req chatRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		badRequest(w, "a question needs some text")
		return
	}
	if err := s.chatMgr.Ask(r.Context(), r.PathValue("id"), strings.TrimSpace(req.Message)); err != nil {
		// One session answers both this screen and the questions asked from a
		// review, and its output carries nobody's name, so the two take turns.
		// Being second is not a fault and is over in a moment: say so rather
		// than reporting an internal error.
		if errors.Is(err, chat.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "asked"})
}

// taskDetail is a finished task's account of itself.
type taskDetail struct {
	Task    *store.Task      `json:"task"`
	History []taskStep       `json:"history"`
	Usage   store.UsageTotal `json:"usage"`
}

type taskStep struct {
	store.TrailStep
	// Subject is the commit's first line. The body says what a role decided;
	// the subject says what it committed, and the two are rarely the same
	// sentence.
	Subject string `json:"subject,omitempty"`
}

// taskDetail answers "what actually happened here", which a lane called Done
// cannot.
func (s *Server) taskDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.db.GetTask(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	history, err := s.db.TaskTrail(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	usage, err := s.db.UsageForTask(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Commit subjects come from the repository, so the view can show what was
	// committed alongside what was said about it.
	var hat *hatchery.Hatchery
	if project, err := s.db.GetProject(r.Context(), task.ProjectID); err == nil {
		hat = hatchery.New(project.Path)
	}

	steps := make([]taskStep, 0, len(history))
	for _, h := range history {
		step := taskStep{TrailStep: h}
		if hat != nil && h.Commit != "" {
			step.Subject = hat.Subject(r.Context(), h.Commit)
		}
		steps = append(steps, step)
	}

	writeJSON(w, http.StatusOK, taskDetail{Task: task, History: steps, Usage: usage})
}

// taskEvents is one step's transcript: what a role actually did while it held
// the work.
//
// Bounded by the window the trail gives it rather than returning a card's whole
// history, because that is the question being asked. Events are the tier that
// ages out (ARCHITECTURE §12.1), so an empty answer is an ordinary one and the
// caller says so rather than showing an empty box.
func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request) {
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	from, err := optionalTime(q.Get("from"))
	if err != nil {
		badRequest(w, "from is not a time: "+q.Get("from"))
		return
	}
	until, err := optionalTime(q.Get("until"))
	if err != nil {
		badRequest(w, "until is not a time: "+q.Get("until"))
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = stepTranscriptLimit
	}
	// One more than asked for, so a full page can be told from a page that
	// happens to end. A transcript cut at five hundred rows reads exactly like
	// a step that made five hundred calls and stopped.
	events, err := s.db.ListEvents(r.Context(), store.EventQuery{
		ProjectID: task.ProjectID,
		Task:      task.ID,
		Role:      q.Get("role"),
		From:      from,
		Until:     until,
		Limit:     limit + 1,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	truncated := len(events) > limit
	if truncated {
		events = events[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":    orEmpty(events),
		"truncated": truncated,
	})
}

// stepTranscriptLimit bounds one step's transcript. A step is minutes of one
// role's work, so this is generous; the flag beside it is what matters.
const stepTranscriptLimit = 500

func optionalTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, v)
}

type integrationRequest struct {
	Integration string `json:"integration"`

	// A pointer, so "not mentioned" and "set to false" stay different answers.
	// As a plain bool, {"integration":"pr"} — the exact body this endpoint took
	// before draft PRs existed, and what any script or cached bundle still
	// sends — decoded as false and turned a project's draft setting off as a
	// side effect of confirming its integration mode.
	PRDraft *bool `json:"prDraft"`
}

// renameProject changes the label a project is shown under.
func (s *Server) renameProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	project, err := s.db.SetProjectName(r.Context(), r.PathValue("id"), req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

// setProjectIcon sets or clears the mark the switcher shows for a project.
//
// PUT rather than POST: sending the same icon twice leaves the same state.
func (s *Server) setProjectIcon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Icon string `json:"icon"`
	}
	if !decode(w, r, &req) {
		return
	}
	project, err := s.db.SetProjectIcon(r.Context(), r.PathValue("id"), req.Icon)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

// setIntegration changes how a project's finished work reaches its base branch.
func (s *Server) setIntegration(w http.ResponseWriter, r *http.Request) {
	var req integrationRequest
	if !decode(w, r, &req) {
		return
	}
	// Absent leaves the stored value alone.
	draft := false
	if req.PRDraft != nil {
		draft = *req.PRDraft
	} else {
		current, err := s.db.GetProject(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		draft = current.PRDraft
	}
	project, err := s.db.SetIntegration(r.Context(), r.PathValue("id"), req.Integration, draft)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

// approvalDiff serves what a pending approval is asking about.
//
// The note says what the role decided; this is what it actually wrote. Deciding
// from the note alone means approving a description of a change rather than the
// change — which for a planner's spec is the whole deliverable.
//
// Lazily, on its own endpoint: most approvals are read, not all are expanded,
// and a diff is far larger than everything else on the card.
func (s *Server) approvalDiff(w http.ResponseWriter, r *http.Request) {
	approval, err := s.db.GetApproval(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.db.GetProject(r.Context(), approval.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if approval.Commit == "" {
		writeJSON(w, http.StatusOK, map[string]any{"diff": "", "truncated": false})
		return
	}

	// Per file, with content as well as diff. A spec is a file the commit
	// created, and its diff is the document with a plus in front of every line
	// — which is not how anyone reads a document they are being asked to
	// approve.
	const maxFile = 256 * 1024

	// How many files are read in full before the rest are left to be opened.
	// A hundred-file change otherwise reads every file and every diff before
	// anyone has looked at the first one, and a reviewer reads them one at a
	// time by construction now.
	const eagerFiles = 30
	hat := hatchery.New(project.Path)

	// The final gate asks a different question. A hand-off between roles is
	// about what that role just wrote; the approval that lands the work is
	// about everything that would reach the base branch, which is usually
	// several commits by several roles.
	seen, err := s.db.FilesSeen(r.Context(), approval.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var files []hatchery.ChangedFile
	if approval.Terminal {
		files, err = hat.RangeFiles(r.Context(), project.BaseBranch, approval.Commit, maxFile, eagerFiles)
	} else {
		files, err = hat.ChangedFiles(r.Context(), approval.Commit, maxFile, eagerFiles)
	}
	if err != nil {
		// A ref this repository does not have is the operator's problem, not the
		// daemon's: a branch deleted, a worktree pruned by hand, a clone that
		// never had it. Rendered as a 500 it reached them as "internal error"
		// over the approval they were being asked to decide, which says nothing
		// about what to do next.
		//
		// Only that case. Everything else the diff can fail on -- git missing
		// from PATH, an unreadable repository, a cancelled request -- is a
		// server fault, and answering it with "your commit does not exist"
		// would send someone looking in the wrong place entirely.
		if errors.Is(err, hatchery.ErrNoSuchRevision) {
			badRequest(w, fmt.Sprintf(
				"cannot read what this approval changed: %v. The role's branch may have been deleted, or its worktree pruned.",
				err))
			s.log.Warn("an approval points at a revision this repository does not have",
				"approval", approval.ID, "commit", approval.Commit, "err", err)
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files,
		// So the view can say whether this is one commit or a merge.
		"range": approval.Terminal,
		"base":  project.BaseBranch,
		// Where the reader got to last time. With the files rather than in a
		// second request: it is about these files, and an approval read on a
		// phone and finished at a desk should open where it was left.
		"seen": seen,
	})
}

// openProject records that a project was opened, so the picker and the initial
// selection surface what you were last working on.
//
// A POST, not a side effect on GET: reading a project should not change it, and
// the cockpit reads one on every poll.
func (s *Server) openProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.db.TouchProject(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.db.GetProject(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

// setTaskHidden puts a finished card away, or brings it back.
//
// PUT rather than POST: sending the same body twice leaves the same state, and
// the switch that calls this can be flipped by two devices watching one board.
func (s *Server) setTaskHidden(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hidden bool `json:"hidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, r, fmt.Errorf("reading request: %w", err))
		return
	}
	if err := s.db.SetTaskHidden(r.Context(), r.PathValue("id"), req.Hidden); err != nil {
		s.fail(w, r, err)
		return
	}
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// setTaskPinned keeps a card's transcript past the retention window, or hands
// it back to the sweep.
func (s *Server) setTaskPinned(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, r, fmt.Errorf("reading request: %w", err))
		return
	}
	if err := s.db.SetTaskPinned(r.Context(), r.PathValue("id"), req.Pinned); err != nil {
		s.fail(w, r, err)
		return
	}
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// approvalMergeable answers whether the work would land, before deciding that
// it should.
//
// Approving is what merges, and nothing said whether the merge would go
// through. The answer costs nothing: the merge happens in memory, so there is
// no worktree to check out and nothing to clean up when it conflicts.
func (s *Server) approvalMergeable(w http.ResponseWriter, r *http.Request) {
	approval, err := s.db.GetApproval(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.db.GetProject(r.Context(), approval.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if approval.Commit == "" {
		badRequest(w, "this approval carries no commit, so there is nothing to merge")
		return
	}
	// The check has to ask what the landing will ask. Merge mode fast-forwards,
	// so a commit that has fallen behind the base does not land however clean
	// the merge would be; a pull request is merged by the forge, and branch
	// mode lands nothing.
	answer, err := hatchery.New(project.Path).MergeCheck(
		r.Context(), project.BaseBranch, approval.Commit,
		project.Integration == store.IntegrateMerge)
	if err != nil {
		// A ref this repository does not have is the operator's problem: a
		// branch deleted, a worktree pruned, a clone that never had it. Every
		// other failure here is a genuine fault.
		if errors.Is(err, hatchery.ErrNoSuchRevision) {
			badRequest(w, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// ── review ────────────────────────────────────────────────────────────────

// reviewThreads is the conversation about a card's code, anchored to it.
func (s *Server) reviewThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.db.ReviewThreads(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(threads))
}

// openReviewThread starts a thread on a line, with the remark that opened it.
func (s *Server) openReviewThread(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApprovalID string `json:"approvalId"`
		CommitSHA  string `json:"commitSha"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		Body       string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	thread := &store.ReviewThread{
		ProjectID: task.ProjectID,
		TaskID:    task.ID,
		CommitSHA: req.CommitSHA,
		File:      req.File,
		Line:      req.Line,
	}
	if req.ApprovalID != "" {
		thread.ApprovalID = &req.ApprovalID
	}
	// The person reading the diff is the author. An agent's answer arrives on
	// the same thread later, under its own name.
	opened, err := s.db.OpenReviewThread(r.Context(), thread, store.OperatorRole, req.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, opened)
}

// addReviewComment adds a turn to a thread.
func (s *Server) addReviewComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Author == "" {
		req.Author = store.OperatorRole
	}
	if _, err := s.db.AddReviewComment(r.Context(), r.PathValue("id"), req.Author, req.Body); err != nil {
		s.fail(w, r, err)
		return
	}
	thread, err := s.db.ReviewThread(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

// resolveReviewThread settles a thread, or opens it again.
//
// A person only: an agent that could resolve the thread asking about its own
// work would be marking its own homework, and the gate reads this.
func (s *Server) resolveReviewThread(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Resolved bool `json:"resolved"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.db.SetReviewThreadState(r.Context(), r.PathValue("id"), req.Resolved); err != nil {
		s.fail(w, r, err)
		return
	}
	thread, err := s.db.ReviewThread(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

// askAboutTheChange puts a reader's question to the agent, with the code in
// front of it, and lands the answer in the thread where it was asked.
//
// The reviewer is the one reviewing. This is not a second opinion on the change
// and does not decide anything: it exists so a person can get a change into
// their head, from inside the diff, without going to find someone. So the
// thread it opens is a question rather than a remark, and holds nothing at the
// gate. If what comes back matters, the reader raises it.
func (s *Server) askAboutTheChange(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		writeError(w, http.StatusNotImplemented, "this build has no agent to ask")
		return
	}
	var req struct {
		ThreadID   string `json:"threadId"`
		ApprovalID string `json:"approvalId"`
		CommitSHA  string `json:"commitSha"`
		Base       string `json:"base"`
		File       string `json:"file"`
		Line       int    `json:"line"`
		// Hunk is the lines the reader is looking at. Sent rather than fetched
		// so the agent is asked about what is on the reader's screen.
		Hunk     string `json:"hunk"`
		Question string `json:"question"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		badRequest(w, "a question needs some text")
		return
	}
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	thread := &store.ReviewThread{}
	if req.ThreadID != "" {
		// A follow-up belongs on the thread that started it.
		if _, err := s.db.AddReviewComment(r.Context(), req.ThreadID, store.OperatorRole, req.Question); err != nil {
			s.fail(w, r, err)
			return
		}
		if thread, err = s.db.ReviewThread(r.Context(), req.ThreadID); err != nil {
			s.fail(w, r, err)
			return
		}
	} else {
		thread.ProjectID, thread.TaskID = task.ProjectID, task.ID
		thread.CommitSHA, thread.File, thread.Line = req.CommitSHA, req.File, req.Line
		thread.Kind = store.ThreadQuestion
		if req.ApprovalID != "" {
			thread.ApprovalID = &req.ApprovalID
		}
		if thread, err = s.db.OpenReviewThread(r.Context(), thread, store.OperatorRole, req.Question); err != nil {
			s.fail(w, r, err)
			return
		}
	}

	// Answered in the background, and the thread comes back now. An agent turn
	// is tens of seconds; a request held open for that is a request that dies
	// on a phone changing networks, and the answer would die with it.
	go s.answerInThread(thread.ID, task.ProjectID, askPrompt(req.Base, req.CommitSHA, req.File, req.Line, req.Hunk, req.Question))

	writeJSON(w, http.StatusAccepted, thread)
}

// answerInThread waits for the agent and writes what it said as a comment.
func (s *Server) answerInThread(threadID, projectID, prompt string) {
	// Its own context: the request that asked is already answered, and its
	// cancellation would take the agent's turn with it.
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()

	answer, err := s.chatMgr.AskAndWait(ctx, projectID, prompt)
	if err != nil && answer == "" {
		answer = "I could not answer that: " + err.Error()
	}

	// A context of its own for the write. The one above is what bounds the
	// agent, and on a timeout it is already cancelled: writing through it threw
	// away both the partial answer and the sentence explaining what happened,
	// so a question that timed out left a thread that had never been answered
	// and no record of why.
	write, cancelWrite := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWrite()
	if _, err := s.db.AddReviewComment(write, threadID, chat.Role, answer); err != nil {
		s.log.Error("review: the answer could not be recorded", "thread", threadID, "err", err)
	}
}

// askTimeout bounds one answer. Long enough for an agent to read the files
// around a hunk, short enough that a thread does not sit empty for ever.
const askTimeout = 3 * time.Minute

// askPrompt puts the reader's question in front of the code it is about.
//
// The hunk, the file and the range, because a question about "this" means
// nothing without them; and an instruction to answer rather than review,
// because the person reading is the reviewer and an agent that volunteers a
// verdict is answering a question nobody asked.
func askPrompt(base, commit, file string, line int, hunk, question string) string {
	var b strings.Builder
	b.WriteString("A person is reviewing a change and has a question about it.\n\n")
	if file != "" {
		fmt.Fprintf(&b, "File: %s", file)
		if line > 0 {
			fmt.Fprintf(&b, ", around line %d", line)
		}
		b.WriteString("\n")
	}
	if base != "" && commit != "" {
		fmt.Fprintf(&b, "The change is %s..%s in this repository.\n", base, commit)
	}
	if strings.TrimSpace(hunk) != "" {
		b.WriteString("\nWhat they are looking at:\n\n```\n")
		b.WriteString(strings.TrimRight(hunk, "\n"))
		b.WriteString("\n```\n")
	}
	fmt.Fprintf(&b, "\nTheir question: %s\n", strings.TrimSpace(question))
	b.WriteString(`
Answer that question and nothing else. Read the surrounding code before you
answer. They are reviewing this change, not you: do not give a verdict on it,
do not list unrelated problems, and do not approve or reject anything. If the
answer is that something is wrong, say what and why, and leave the decision to
them. Keep it to a few sentences.`)
	return b.String()
}

// raiseReviewThread turns a question into a remark, which is the reader
// deciding that what they learned has to be dealt with before this lands.
func (s *Server) raiseReviewThread(w http.ResponseWriter, r *http.Request) {
	if err := s.db.RaiseReviewThread(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	thread, err := s.db.ReviewThread(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

// approvalFile reads one file of a change, for the ones the listing left alone.
func (s *Server) approvalFile(w http.ResponseWriter, r *http.Request) {
	approval, err := s.db.GetApproval(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.db.GetProject(r.Context(), approval.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" || approval.Commit == "" {
		badRequest(w, "which file, of which commit?")
		return
	}
	base := ""
	if approval.Terminal {
		base = project.BaseBranch
	}
	file, err := hatchery.New(project.Path).LoadFile(r.Context(), base, approval.Commit, path, 256*1024)
	if err != nil {
		if errors.Is(err, hatchery.ErrNoSuchRevision) {
			badRequest(w, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

// markFileSeen records where a reader has got to in a diff.
func (s *Server) markFileSeen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
		Seen bool   `json:"seen"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.File == "" {
		badRequest(w, "which file?")
		return
	}
	if err := s.db.MarkFileSeen(r.Context(), r.PathValue("id"), req.File, req.Seen); err != nil {
		s.fail(w, r, err)
		return
	}
	seen, err := s.db.FilesSeen(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seen": seen})
}

// stopTask parks a card so nothing picks it up again.
func (s *Server) stopTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.db.GetTask(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.StopTask(r.Context(), task.ProjectID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	// The role holding it does not read the queue while it is working, so being
	// told is the only way it finds out. Cooperative on purpose — see
	// Overmind.Interrupt — and the card is already stopped either way.
	if s.over != nil && task.Lane != "" && task.Lane != store.LaneDone {
		s.over.Interrupt(task.ProjectID, task.Lane, fmt.Sprintf(
			"The operator stopped the card %q. Stop working on it now: do not commit "+
				"anything further for it, and do not run `zerg send` or `zerg done` for it, since "+
				"both are refused for a stopped card. Run `zerg next` to claim other work.",
			task.Name))
	}
	updated, err := s.db.GetTask(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deleteTask removes a card and the transcript that only makes sense beside it.
// Usage rows are kept — see store.DeleteTask.
func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.db.GetTask(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.db.DeleteTask(r.Context(), task.ProjectID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetChat ends a conversation and removes its worktree and transcript.
func (s *Server) resetChat(w http.ResponseWriter, r *http.Request) {
	if s.chatMgr == nil {
		s.fail(w, r, fmt.Errorf("chat is not available in this build"))
		return
	}
	if err := s.chatMgr.Reset(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setChatAgent chooses what answers questions. Empty values mean inherit from
// the terminal role, which is the default.
func (s *Server) setChatAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Harness string `json:"harness"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, r, fmt.Errorf("reading request: %w", err))
		return
	}
	if req.Harness != "" {
		if _, err := s.registry.Get(req.Harness); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	project, err := s.db.SetChatAgent(r.Context(), r.PathValue("id"), req.Harness, req.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The running session was built from the old choice, so end it. The next
	// question starts one that matches what the setting now says.
	if s.chatMgr != nil {
		s.chatMgr.Stop(r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, project)
}

// workspace reports what this project's worktrees occupy.
//
// Separate from the board poll: walking several checkouts is real filesystem
// work and the answer moves in megabytes over minutes, so it is fetched on its
// own slower cadence rather than every two seconds.
func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, hatchery.New(project.Path).Measure())
}
