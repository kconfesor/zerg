package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/konfessor/zerg/internal/hatchery"
	"github.com/konfessor/zerg/internal/store"
	"github.com/konfessor/zerg/internal/tailnet"
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

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetConfig(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:        cfg,
		Tailnet:       tailnet.Probe(r.Context()),
		Applied:       s.applied,
		RestartNeeded: s.applied != "" && s.applied != cfg.Addr,
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
		Applied:       s.applied,
		RestartNeeded: s.applied != "" && s.applied != saved.Addr,
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
}

// setIntegration changes how a project's finished work reaches its base branch.
func (s *Server) setIntegration(w http.ResponseWriter, r *http.Request) {
	var req integrationRequest
	if !decode(w, r, &req) {
		return
	}
	project, err := s.db.SetIntegration(r.Context(), r.PathValue("id"), req.Integration)
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
	files, err := hatchery.New(project.Path).ChangedFiles(r.Context(), approval.Commit, maxFile)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
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
