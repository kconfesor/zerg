package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kconfesor/zerg/internal/preview"
	"github.com/kconfesor/zerg/internal/store"
)

// Running the work here, so a person can click it.
//
// The endpoint behind the button at the approval gate. It answers when the
// thing is up or when it failed, because those are the two things whoever
// pressed it is waiting to find out, and a 202 with a spinner would only move
// the waiting somewhere less useful.

// deployTargets lists a project's targets.
func (s *Server) deployTargets(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.DeployTargets(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(list))
}

// saveDeployTarget creates or updates one.
func (s *Server) saveDeployTarget(w http.ResponseWriter, r *http.Request) {
	var t store.DeployTarget
	if !decode(w, r, &t) {
		return
	}
	t.ProjectID = r.PathValue("id")
	if t.Kind == "" {
		t.Kind = store.TargetLocal
	}
	saved, err := s.db.SaveDeployTarget(r.Context(), &t)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// deleteDeployTarget removes one.
func (s *Server) deleteDeployTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DeleteDeployTarget(r.Context(), r.PathValue("target")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startPreview runs a target at a commit and answers with the service.
func (s *Server) startPreview(w http.ResponseWriter, r *http.Request) {
	if s.preview == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	var req struct {
		TargetID string `json:"targetId"`
		Commit   string `json:"commit"`
		TaskID   string `json:"taskId"`
	}
	if !decode(w, r, &req) {
		return
	}

	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	target, err := s.resolveTarget(r, project.ID, req.TargetID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Commit == "" {
		// The commit is the point: a preview of "whatever is checked out" is a
		// preview of something nobody chose.
		badRequest(w, "a preview needs the commit to run")
		return
	}

	a, err := s.preview.Start(r.Context(), project, target, req.Commit, req.TaskID)
	if err != nil {
		var failed *preview.StartError
		if errors.As(err, &failed) {
			// Everything about this is the operator's to fix -- a command that
			// does not build, a port it did not bind, a compose file that is
			// not there -- and the output is where the reason is. It travels
			// with the error rather than only into the daemon's log.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": failed.Error(),
				"log":   failed.Log,
			})
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, artifactOut{Artifact: *a, URL: ServiceURL(r, s.proxyPort, a.ID)})
}

// stopPreview ends a project's preview.
func (s *Server) stopPreview(w http.ResponseWriter, r *http.Request) {
	if s.preview == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	s.preview.Stop(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// previewLog is what the running preview has printed, for somebody watching a
// build that is taking its time or reading why one stopped.
func (s *Server) previewLog(w http.ResponseWriter, r *http.Request) {
	if s.preview == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": s.preview.Log(r.PathValue("id"))})
}

// operatorError is a problem the person can fix, carried as an error so it can
// be returned from a helper rather than written at every call site.
//
// The Validation marker is what fail() looks for, the same one the store's own
// validation errors carry: "this project has no target yet" and "that name is
// taken" are the same kind of answer and should reach the caller as the same
// status.
type operatorError struct{ msg string }

func (e *operatorError) Error() string { return e.msg }
func (e *operatorError) Validation()   {}

func invalidRequest(format string, args ...any) error {
	return &operatorError{msg: fmt.Sprintf(format, args...)}
}

// resolveTarget picks the target to run: the one asked for, or the project's
// only local one.
//
// Named or obvious, never guessed among several. A project with two targets
// and no id in the request is a question, and picking one of them would be
// answering it on the operator's behalf.
func (s *Server) resolveTarget(r *http.Request, projectID, id string) (*store.DeployTarget, error) {
	if id != "" {
		t, err := s.db.GetDeployTarget(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if t.ProjectID != projectID {
			return nil, invalidRequest("that target belongs to another project")
		}
		return t, nil
	}

	list, err := s.db.DeployTargets(r.Context(), projectID)
	if err != nil {
		return nil, err
	}
	var local []store.DeployTarget
	for _, t := range list {
		if t.Kind == store.TargetLocal {
			local = append(local, t)
		}
	}
	switch len(local) {
	case 0:
		return nil, invalidRequest(
			"this project has nothing to run yet: add a target in Settings, a command that " +
				"serves the app on $PORT")
	case 1:
		return &local[0], nil
	default:
		return nil, invalidRequest("this project has %d local targets; say which one", len(local))
	}
}
