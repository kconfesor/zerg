// Package api serves the cockpit's REST surface.
//
// Commands are request/response with a status code, so they live on HTTP
// rather than on the event WebSocket (ARCHITECTURE.md §7.5). A rejected
// update is a 400 with a readable reason, not a hand-rolled error frame.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/preflight"
	"github.com/konfessor/zerg/internal/store"
)

// Server holds the dependencies every handler needs.
type Server struct {
	db       *store.DB
	log      *slog.Logger
	registry *adapter.Registry
	preflt   *preflight.Runner
}

func New(db *store.DB, log *slog.Logger, reg *adapter.Registry) *Server {
	return &Server{
		db:       db,
		log:      log,
		registry: reg,
		preflt:   preflight.NewRunner(db, reg),
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

	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)

	// The team: which library roles this project uses, in what order.
	mux.HandleFunc("GET /api/projects/{id}/team", s.getTeam)
	mux.HandleFunc("PUT /api/projects/{id}/team", s.setTeam)

	// What this build can drive, and what a role may be pointed at.
	mux.HandleFunc("GET /api/harnesses", s.listHarnesses)
	mux.HandleFunc("GET /api/harnesses/{name}/models", s.listModels)

	// The readiness gate: a team that cannot work must not reach a board.
	mux.HandleFunc("GET /api/projects/{id}/readiness", s.readiness)

	mux.HandleFunc("GET /api/settings/shared-instructions", s.getSharedInstructions)
	mux.HandleFunc("PUT /api/settings/shared-instructions", s.setSharedInstructions)

	return s.withLogging(mux)
}

// ── health ────────────────────────────────────────────────────────────────

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	writeJSON(w, http.StatusOK, &t)
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteTemplate(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
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

	p, err := s.db.CreateProject(r.Context(), req.Path, req.Name, req.BaseBranch)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A new project arrives with a working pipeline rather than an empty one,
	// so adding a repo is two clicks and not a configuration session.
	if err := s.db.SelectDefaultTeam(r.Context(), p.ID); err != nil {
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

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteProject(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── team ──────────────────────────────────────────────────────────────────

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	team, err := s.db.ResolveTeam(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(team))
}

// setTeam takes the whole desired pipeline rather than a diff: a reorder and a
// selection change are the same operation, and sending the whole thing means
// they cannot half-apply.
func (s *Server) setTeam(w http.ResponseWriter, r *http.Request) {
	var roles []store.ProjectRole
	if !decode(w, r, &roles) {
		return
	}
	id := r.PathValue("id")
	if err := s.db.SetTeam(r.Context(), id, roles); err != nil {
		s.fail(w, r, err)
		return
	}
	team, err := s.db.ResolveTeam(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(team))
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

// orEmpty renders a nil slice as [] rather than null, so a client never has to
// special-case "no rows yet".
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
