package api

import (
	"errors"
	"net/http"

	"github.com/kconfesor/zerg/internal/runner"
	"github.com/kconfesor/zerg/internal/store"
)

// Running a project so somebody can look at it.
//
// The daemon does not run anything here: it starts an agent, which reads the
// repository and works out how the project serves itself. These endpoints are
// the conversation with that agent -- start it, correct it, stop it, and read
// what it learned.

// runStatus is what the panel polls: the state, and what a previous runner
// wrote down about this project.
type runStatus struct {
	runner.Status
	// Note is what has been learned about running this project, empty until a
	// runner has been anywhere.
	Note string `json:"note,omitempty"`
	// NoteAuthor is who last wrote it: the runner, or the operator correcting
	// it. Shown, because "the agent thinks this" and "you told it this" are
	// different claims.
	NoteAuthor string `json:"noteAuthor,omitempty"`
	// Services is what is being served, with an address. The panel said
	// "running" and stopped there, which answers the wrong question: the
	// reason to run a preview is to open it.
	Services []liveService `json:"services"`
}

func (s *Server) runState(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	id := r.PathValue("id")
	out := runStatus{Status: s.runner.Status(id)}

	if note, err := s.db.RunNoteFor(r.Context(), id); err == nil {
		out.Note, out.NoteAuthor = note.Note, note.Author
	}
	out.Services = s.liveServices(r, id)
	writeJSON(w, http.StatusOK, out)
}

// startRun asks the runner to serve a commit.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	var req struct {
		Commit string `json:"commit"`
		TaskID string `json:"taskId"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.runner.Run(r.Context(), r.PathValue("id"), req.Commit, req.TaskID); err != nil {
		// A project with no harness, a commit this repository does not have, a
		// worktree that cannot be made: all things the operator can fix, and
		// all of them named rather than reported as a fault.
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.runner.Status(r.PathValue("id")))
}

// guideRun tells the running agent something.
//
// The reason its session stays alive: "no, the admin portal" reaches an agent
// that still remembers what it just tried, which is cheaper and likelier to
// work than starting again from nothing.
func (s *Server) guideRun(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.runner.Guide(r.Context(), r.PathValue("id"), req.Text); err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.runner.Status(r.PathValue("id")))
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot run previews")
		return
	}
	s.runner.Stop(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// touchRun says somebody is still looking, which is what the idle timer
// measures. Sent when the frame is opened.
func (s *Server) touchRun(w http.ResponseWriter, r *http.Request) {
	if s.runner != nil {
		s.runner.Touch(r.PathValue("id"))
	}
	w.WriteHeader(http.StatusNoContent)
}

// saveRunNote corrects what the agent believes about this project.
//
// The operator's version wins until a runner learns something newer, and the
// author is recorded so the next runner is told which it is reading.
func (s *Server) saveRunNote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.db.SaveRunNote(r.Context(), r.PathValue("id"), req.Note, store.OperatorRole); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
