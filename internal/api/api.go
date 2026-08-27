// Package api serves the cockpit's REST surface.
//
// Commands are request/response with a status code, so they live on HTTP
// rather than on the event WebSocket (ARCHITECTURE.md §7.5). A rejected
// update is a 400 with a readable reason, not a hand-rolled error frame.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/chat"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/overmind"
	"github.com/kconfesor/zerg/internal/preflight"
	"github.com/kconfesor/zerg/internal/store"
)

// Server holds the dependencies every handler needs.
type Server struct {
	db       *store.DB
	log      *slog.Logger
	registry *adapter.Registry
	preflt   *preflight.Runner
	over     *overmind.Overmind
	nyd      *nydus.Nydus
	bus      *event.Bus
	recorder *event.Recorder

	// applied is the listener configuration this process actually bound, so the
	// UI can tell a saved setting from a running one.
	applied store.Listener

	chatMgr *chat.Manager

	// ui is an override for the embedded cockpit; see Deps.UI.
	ui http.Handler
}

// Deps are what the API needs to serve the cockpit.
type Deps struct {
	DB        *store.DB
	Log       *slog.Logger
	Registry  *adapter.Registry
	Preflight *preflight.Runner
	Overmind  *overmind.Overmind
	Nydus     *nydus.Nydus

	// Bus is what the activity stream tails. Without it /events replies 503
	// rather than serving an empty stream that looks like a quiet project.
	Bus *event.Bus

	// Recorder is optional; health reports its lag and losses when present.
	Recorder *event.Recorder

	// Applied is the listener configuration the daemon bound at startup.
	Applied store.Listener

	// UI replaces the embedded cockpit, which is how the dev server is put in
	// front of it: same origin, same API, hot reload. Nil serves what was
	// compiled in, which is what a released binary does.
	UI http.Handler

	// Chat answers questions about a project without touching the pipeline.
	Chat *chat.Manager
}

func New(d Deps) *Server {
	pf := d.Preflight
	if pf == nil {
		pf = preflight.NewRunner(d.DB, d.Registry)
	}
	return &Server{
		db: d.DB, log: d.Log, registry: d.Registry,
		preflt: pf, over: d.Overmind, nyd: d.Nydus, bus: d.Bus, applied: d.Applied, chatMgr: d.Chat,
		recorder: d.Recorder, ui: d.UI,
	}
}

// Routes builds the mux. Go 1.22+ patterns carry the method and the wildcard,
// which is enough structure that a third-party router earns nothing here.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.health)

	// The library: global, shared by every project.
	mux.HandleFunc("GET /api/roles", s.listRoles)
	mux.HandleFunc("POST /api/roles", s.createRole)
	mux.HandleFunc("GET /api/roles/{id}", s.getRole)
	mux.HandleFunc("PUT /api/roles/{id}", s.updateRole)
	mux.HandleFunc("DELETE /api/roles/{id}", s.deleteRole)
	mux.HandleFunc("GET /api/team-presets", s.listTeamPresets)
	mux.HandleFunc("POST /api/team-presets", s.createTeamPreset)
	mux.HandleFunc("GET /api/team-presets/{id}", s.getTeamPreset)
	mux.HandleFunc("PUT /api/team-presets/{id}", s.updateTeamPreset)
	mux.HandleFunc("DELETE /api/team-presets/{id}", s.deleteTeamPreset)

	// Listing directories so adding a project is picking a folder, not typing
	// its path. Read-only, and on the daemon's filesystem because that is the
	// side the repositories live on, not the browser's.
	mux.HandleFunc("GET /api/browse", s.browse)

	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("POST /api/projects/{id}/open", s.openProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)

	// The team: which library roles this project uses, in what order.
	mux.HandleFunc("GET /api/projects/{id}/team", s.getTeam)
	mux.HandleFunc("PUT /api/projects/{id}/team", s.setTeam)

	// What this build can drive, and what a role may be pointed at.
	mux.HandleFunc("GET /api/harnesses", s.listHarnesses)
	mux.HandleFunc("GET /api/harnesses/{name}/models", s.listModels)

	// The readiness gate: a team that cannot work must not reach a board.
	mux.HandleFunc("GET /api/projects/{id}/readiness", s.readiness)

	// Running a swarm.
	mux.HandleFunc("POST /api/projects/{id}/start", s.start)
	mux.HandleFunc("POST /api/projects/{id}/stop", s.stop)
	mux.HandleFunc("GET /api/projects/{id}/status", s.status)

	// The board and what needs a human.
	mux.HandleFunc("GET /api/projects/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /api/projects/{id}/tasks", s.newTask)
	mux.HandleFunc("GET /api/projects/{id}/attention", s.attention)
	mux.HandleFunc("GET /api/projects/{id}/usage", s.usage)
	mux.HandleFunc("GET /api/projects/{id}/spend", s.spend)
	mux.HandleFunc("GET /api/projects/{id}/stream", s.stream)
	mux.HandleFunc("GET /api/tasks/{id}", s.taskDetail)
	mux.HandleFunc("GET /api/tasks/{id}/usage", s.taskUsage)
	mux.HandleFunc("PUT /api/tasks/{id}/hidden", s.setTaskHidden)
	mux.HandleFunc("POST /api/tasks/{id}/stop", s.stopTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.deleteTask)
	mux.HandleFunc("GET /api/approvals/{id}/diff", s.approvalDiff)
	mux.HandleFunc("POST /api/approvals/{id}/approve", s.approve)
	mux.HandleFunc("POST /api/approvals/{id}/reject", s.reject)
	mux.HandleFunc("POST /api/clarifications/{id}/answer", s.answer)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.setSettings)
	mux.HandleFunc("POST /api/projects/{id}/sweep", s.sweep)
	mux.HandleFunc("GET /api/projects/{id}/workspace", s.workspace)
	mux.HandleFunc("PUT /api/projects/{id}/integration", s.setIntegration)
	mux.HandleFunc("PUT /api/projects/{id}/name", s.renameProject)
	mux.HandleFunc("PUT /api/projects/{id}/icon", s.setProjectIcon)
	mux.HandleFunc("GET /api/projects/{id}/icons", s.projectIcons)
	mux.HandleFunc("GET /api/projects/{id}/icon", s.projectIcon)
	mux.HandleFunc("POST /api/projects/{id}/chat", s.askChat)
	mux.HandleFunc("DELETE /api/projects/{id}/chat", s.resetChat)
	mux.HandleFunc("PUT /api/projects/{id}/chat-agent", s.setChatAgent)
	mux.HandleFunc("GET /api/settings/shared-instructions", s.getSharedInstructions)
	mux.HandleFunc("PUT /api/settings/shared-instructions", s.setSharedInstructions)

	// The API namespace is claimed here, on the mux, not inside the cockpit
	// handler that used to guard it.
	//
	// With the dev server in front, "everything the API did not claim" reached
	// Vite, whose own config proxies /api straight back to this daemon. An
	// unknown endpoint therefore bounced zerg to Vite to zerg to Vite until
	// something gave out. Claiming the namespace means a wrong URL is answered
	// once, with the 404 the caller can act on, whichever UI is mounted.
	mux.HandleFunc("/api", s.noSuchEndpoint)
	mux.HandleFunc("/api/", s.noSuchEndpoint)

	// The cockpit itself, on everything the API did not claim.
	if s.ui != nil {
		mux.Handle("/", s.ui)
	} else if ui, err := cockpit(); err != nil {
		s.log.Error("the cockpit could not be served", "err", err)
	} else {
		mux.Handle("/", ui)
	}

	return s.guard(s.withLogging(mux))
}

// ── health ────────────────────────────────────────────────────────────────

// health reports whether the record is keeping up, not only that the process
// is answering. A recorder silently behind is exactly the failure that used to
// be invisible: transcripts and usage rows go missing while every endpoint
// reports ok.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"status": "ok"}
	if s.recorder != nil {
		st := s.recorder.Stats()
		body["recorder"] = st
		if st.Dropped > 0 || st.Failed > 0 {
			body["status"] = "degraded"
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// ── roles ─────────────────────────────────────────────────────────────────

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.db.ListTemplates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(roles))
}

func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	var t store.RoleTemplate
	if !decode(w, r, &t) {
		return
	}
	// builtin marks what shipped with zerg; a client cannot claim it.
	t.Builtin = false
	created, err := s.db.CreateTemplate(r.Context(), &t)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getRole(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.GetTemplate(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.db.GetTemplate(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var t store.RoleTemplate
	if !decode(w, r, &t) {
		return
	}
	// The path names the row; the body cannot move it, and it cannot promote
	// itself to a built-in.
	t.ID = id
	t.Builtin = existing.Builtin

	if err := s.db.UpdateTemplate(r.Context(), &t); err != nil {
		s.fail(w, r, err)
		return
	}
	// A library edit can change the one thing a running cerebrate cannot
	// re-read: which harness it is. Model, prompt and arguments are picked up
	// at the next spawn, but the adapter and the private config directory are
	// fixed when the process is created, so a harness change left the old
	// harness running with the new one's model and flags.
	s.reconcileRunning(r.Context())
	writeJSON(w, http.StatusOK, &t)
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteTemplate(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	// The template's team memberships went with it (ON DELETE CASCADE), so
	// every running project that used it now has a process for a role that no
	// longer exists.
	s.reconcileRunning(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ── reusable team presets ─────────────────────────────────────────────────

func (s *Server) listTeamPresets(w http.ResponseWriter, r *http.Request) {
	teams, err := s.db.ListTeamPresets(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(teams))
}

func (s *Server) createTeamPreset(w http.ResponseWriter, r *http.Request) {
	var p store.TeamPreset
	if !decode(w, r, &p) {
		return
	}
	p.Builtin = false
	created, err := s.db.CreateTeamPreset(r.Context(), &p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getTeamPreset(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.GetTeamPreset(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) updateTeamPreset(w http.ResponseWriter, r *http.Request) {
	existing, err := s.db.GetTeamPreset(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var p store.TeamPreset
	if !decode(w, r, &p) {
		return
	}
	p.ID, p.Builtin = existing.ID, existing.Builtin
	if err := s.db.UpdateTeamPreset(r.Context(), &p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reconcileRunning(r.Context())
	updated, err := s.db.GetTeamPreset(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteTeamPreset(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteTeamPreset(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reconcileRunning(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ── projects ──────────────────────────────────────────────────────────────

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.db.ListProjects(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(ps))
}

type createProjectRequest struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	BaseBranch string `json:"baseBranch"`
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Path == "" {
		badRequest(w, "a project needs a path")
		return
	}

	// A new project arrives with a working pipeline rather than an empty one,
	// so adding a repo is two clicks and not a configuration session — and the
	// two are written together, so a library missing a built-in role leaves no
	// project row behind holding a path nobody can reuse.
	p, err := s.db.CreateProjectWithDefaultTeam(r.Context(), req.Path, req.Name, req.BaseBranch)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// deleteProject forgets a project, and refuses while its swarm is up.
//
// Deleting used to go straight to the database. The rows went — tasks,
// messages, leases, the session — while the agents kept running, holding
// worktrees and tokens for a project that no longer existed, writing events
// with a project id nothing could resolve and failing every claim. Nothing
// stopped them, because the only thing that could stop them was reached
// through the project the delete had just removed.
//
// Refusing rather than stopping the swarm on the caller's behalf: taking a
// running pipeline down is a decision, and the Stop button is one click away.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.over != nil && s.over.Running(id) {
		writeError(w, http.StatusConflict,
			"this project's swarm is running; stop it before deleting the project")
		return
	}
	if err := s.db.DeleteProject(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── team ──────────────────────────────────────────────────────────────────

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	team, err := s.db.GetProjectTeam(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, team)
}

// setTeam takes the whole desired pipeline rather than a diff: a reorder and a
// selection change are the same operation, and sending the whole thing means
// they cannot half-apply.
func (s *Server) setTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PresetID         *string             `json:"presetId"`
		TopologyOverride bool                `json:"topologyOverride"`
		Roles            []store.ProjectRole `json:"roles"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.db.SetProjectTeam(r.Context(), id, req.PresetID, req.TopologyOverride, req.Roles); err != nil {
		s.fail(w, r, err)
		return
	}
	// And the running swarm follows the edit. Writing the team and stopping
	// there meant the change reached only whichever roles happened to respawn:
	// an added role had no process, so work routed to it queued behind nothing;
	// a removed one kept working until it next crashed.
	if err := s.reconcile(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	team, err := s.db.GetProjectTeam(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, team)
}

// ── harnesses ─────────────────────────────────────────────────────────────

func (s *Server) listHarnesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, orEmpty(s.registry.Names()))
}

// listModels asks the harness what it can actually run, so the role editor
// offers a picker instead of a text box. The field still accepts free text —
// a catalog can lag a working model — but a hand-typed id that 400s on every
// turn is the failure this exists to prevent.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	a, err := s.registry.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	models, err := a.ListModels(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(models))
}

// readiness runs every check across every enabled role. The cockpit disables
// Start while Ready is false.
func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	report, err := s.preflt.Check(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// ── running a swarm ───────────────────────────────────────────────────────

// start brings a project up, refusing when the team cannot work.
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if s.over == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run swarms")
		return
	}
	err := s.over.Start(r.Context(), r.PathValue("id"))

	var notReady *overmind.ErrNotReady
	if errors.As(err, &notReady) {
		// 409, with the readiness report attached: the caller needs to know
		// which role failed which check and what fixes it, not merely that
		// something was wrong.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     err.Error(),
			"readiness": notReady.Report,
		})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.statusBody(w, r, r.PathValue("id"))
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if s.over == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run swarms")
		return
	}
	if err := s.over.Stop(r.Context(), r.PathValue("id"), "stopped by the operator"); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.statusBody(w, r, r.PathValue("id"))
}

func (s *Server) statusBody(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.over == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "roles": []any{}})
		return
	}
	roles, err := s.over.Status(r.Context(), projectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body := map[string]any{
		"running": s.over.Running(projectID),
		"roles":   orEmpty(roles),
	}
	// Per harness, not per role: one subscription serves every role on it.
	if q := s.over.Quotas(projectID); len(q) > 0 {
		body["quotas"] = q
	}
	writeJSON(w, http.StatusOK, body)
}

// ── board ─────────────────────────────────────────────────────────────────

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.db.ListTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(tasks))
}

type newTaskRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// newTask opens a card and queues it for the first role in the pipeline.
func (s *Server) newTask(w http.ResponseWriter, r *http.Request) {
	if s.nyd == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot route work")
		return
	}
	var req newTaskRequest
	if !decode(w, r, &req) {
		return
	}
	task, err := s.nyd.NewTask(r.Context(), r.PathValue("id"), req.Name, req.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// ── attention ─────────────────────────────────────────────────────────────

// attention is everything waiting on a human, in one place.
func (s *Server) attention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	approvals, err := s.db.ListPendingApprovals(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	questions, err := s.db.ListOpenClarifications(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Cards that keep going backward. Not a blocked state and not an error —
	// two roles disagreeing about something a human should probably settle,
	// surfaced before the laps become expensive rather than after.
	threshold := s.db.ReworkThreshold(r.Context())
	looping, err := s.db.ListReworkedTasks(r.Context(), id, threshold)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approvals":      orEmpty(approvals),
		"clarifications": orEmpty(questions),
		"rework": map[string]any{
			"threshold": threshold,
			"tasks":     orEmpty(looping),
		},
	})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if s.nyd == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot route work")
		return
	}
	if err := s.nyd.Approve(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

type rejectRequest struct {
	Note string `json:"note"`
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	if s.nyd == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot route work")
		return
	}
	var req rejectRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.nyd.Reject(r.Context(), r.PathValue("id"), req.Note); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

type answerRequest struct {
	Answer string `json:"answer"`
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var req answerRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Answer == "" {
		badRequest(w, "an answer cannot be empty")
		return
	}
	if err := s.db.AnswerClarification(r.Context(), r.PathValue("id"), req.Answer); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "answered"})
}

// ── settings ──────────────────────────────────────────────────────────────

type instructionsBody struct {
	Text string `json:"text"`
}

func (s *Server) getSharedInstructions(w http.ResponseWriter, r *http.Request) {
	text, err := s.db.GetSetting(r.Context(), store.SettingSharedInstructions)
	if errors.Is(err, store.ErrNotFound) {
		text = ""
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, instructionsBody{Text: text})
}

func (s *Server) setSharedInstructions(w http.ResponseWriter, r *http.Request) {
	var body instructionsBody
	if !decode(w, r, &body) {
		return
	}
	if err := s.db.SetSetting(r.Context(), store.SettingSharedInstructions, body.Text); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// ── plumbing ──────────────────────────────────────────────────────────────

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// fail maps a store error onto a status code. Anything that is not a known
// client mistake is logged and reported as a 500 with a generic message —
// internal detail belongs in the log, not in a response body.
// reconcile brings a running swarm in line with what was just written, and
// does nothing when the project is stopped.
func (s *Server) reconcile(ctx context.Context, projectID string) error {
	if s.over == nil {
		return nil
	}
	return s.over.Reconcile(ctx, projectID)
}

// reconcileRunning brings every running swarm in line with the library, for
// edits that are not scoped to one project.
//
// Failures are logged rather than returned: the edit itself succeeded, and a
// role that will not spawn is a state the board already shows.
func (s *Server) reconcileRunning(ctx context.Context) {
	if s.over == nil {
		return
	}
	projects, err := s.db.ListProjects(ctx)
	if err != nil {
		s.log.Warn("could not reconcile running swarms after a library edit", "err", err)
		return
	}
	for _, p := range projects {
		if !s.over.Running(p.ID) {
			continue
		}
		if err := s.over.Reconcile(ctx, p.ID); err != nil {
			s.log.Warn("reconciling a running swarm", "project", p.ID, "err", err)
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case isClientError(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// isClientError recognises the mistakes a caller can fix.
//
// Validation errors carry a marker method, so they are found by type. Database
// constraint violations mean the same thing to a user — "that name is taken" —
// but the driver reports them as strings, so those are matched on the SQLite
// error text rather than left to surface as a 500.
func isClientError(err error) bool {
	var v interface{ Validation() }
	if errors.As(err, &v) {
		return true
	}
	return strings.Contains(err.Error(), "constraint failed")
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		badRequest(w, fmt.Sprintf("could not read the request body: %v", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		slog.Error("writing response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func badRequest(w http.ResponseWriter, msg string) { writeError(w, http.StatusBadRequest, msg) }

// noSuchEndpoint answers anything under /api that no route matched.
//
// JSON rather than the app shell: a mistyped endpoint answered with HTML is a
// response a client parses as neither, so a wrong URL looks like a malformed
// reply for as long as it takes someone to check.
func (s *Server) noSuchEndpoint(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
}

// orEmpty renders a nil slice as [] rather than null, so a client never has to
// special-case "no rows yet".
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ── usage ─────────────────────────────────────────────────────────────────

// usage totals a project's spend, grouped by role, provider or model.
//
// The three groupings answer three different questions and the dashboard shows
// all of them: which stage of the pipeline costs the most, which provider the
// money goes to, and whether an expensive model is earning its price.
// spendResponse is everything the spend view reads, at one grain each.
//
// One request rather than four, because the four are answers about the same
// window and asking separately lets them disagree — a turn recorded between two
// of the calls appears in the roles and not in the providers, and the columns
// stop adding up to the total for reasons nobody can see.
type spendResponse struct {
	Range string `json:"range"`

	// From is when the window opens; absent means all of recorded history.
	From *time.Time `json:"from,omitempty"`

	// SessionStarted distinguishes "this session" from "everything" when the
	// project has never been started, which are the same window and different
	// statements.
	SessionStarted bool `json:"sessionStarted"`

	Roles     []store.RoleUsage  `json:"roles"`
	Providers []store.UsageTotal `json:"providers"`
	Models    []store.UsageTotal `json:"models"`

	// Flags are roles whose cache hit rate has fallen against their own past.
	// The one regression nothing else reports: a changed byte in the composed
	// system prompt invalidates the cache silently, and the bill is the only
	// thing that moves.
	Flags []store.CacheFlag `json:"flags"`
}

// spend serves the whole spend view from one read of usage_turns.
func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.db.GetProject(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = store.RangeSession
	}
	if !store.ValidSpendRange(rng) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown range %q; use session, 24h, 7d, 30d or all", rng))
		return
	}

	from, err := s.db.ResolveSpendRange(r.Context(), id, rng)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	roles, err := s.db.UsageByRole(r.Context(), id, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	providers, err := s.db.UsageByGroup(r.Context(), id, "provider", from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	models, err := s.db.UsageByGroup(r.Context(), id, "model", from)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	flags, err := s.db.CacheRegressions(r.Context(), id, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	out := spendResponse{
		Range:          rng,
		SessionStarted: rng == store.RangeSession && !from.IsZero(),
		Roles:          orEmpty(roles),
		Providers:      orEmpty(providers),
		Models:         orEmpty(models),
		Flags:          orEmpty(flags),
	}
	if !from.IsZero() {
		out.From = &from
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	groupBy := r.URL.Query().Get("by")
	if groupBy == "" {
		groupBy = "role"
	}

	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("since must be an RFC3339 timestamp: %v", err))
			return
		}
		since = t
	}

	if !store.ValidUsageGrouping(groupBy) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("cannot group usage by %q; use role, provider or model", groupBy))
		return
	}

	totals, err := s.db.UsageByGroup(r.Context(), r.PathValue("id"), groupBy, since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(totals))
}

// taskUsage totals one card across every role and every lap it made. With the
// rework counter beside it, this is what a round trip actually cost.
func (s *Server) taskUsage(w http.ResponseWriter, r *http.Request) {
	total, err := s.db.UsageForTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, total)
}
