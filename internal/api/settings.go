package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
	"github.com/kconfesor/zerg/internal/tailnet"
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
	store.Handoff
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
	history, err := s.db.TaskHistory(r.Context(), id)
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
		step := taskStep{Handoff: h}
		if hat != nil && h.Commit != "" {
			step.Subject = hat.Subject(r.Context(), h.Commit)
		}
		steps = append(steps, step)
	}

	writeJSON(w, http.StatusOK, taskDetail{Task: task, History: steps, Usage: usage})
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
	hat := hatchery.New(project.Path)

	// The final gate asks a different question. A hand-off between roles is
	// about what that role just wrote; the approval that lands the work is
	// about everything that would reach the base branch, which is usually
	// several commits by several roles.
	var files []hatchery.ChangedFile
	if approval.Terminal {
		files, err = hat.RangeFiles(r.Context(), project.BaseBranch, approval.Commit, maxFile)
	} else {
		files, err = hat.ChangedFiles(r.Context(), approval.Commit, maxFile)
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
