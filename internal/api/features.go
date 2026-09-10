package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

func (s *Server) featureDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.db.FeatureDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// A file request names the same immutable head as the expanded diff, not
// whichever head happens to exist when its deferred files are opened.
func (s *Server) featureFiles(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.FeatureDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.db.GetProject(r.Context(), d.Task.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	evidence := r.URL.Query().Get("evidence")
	head := r.URL.Query().Get("head")
	known := false
	if evidence != "" {
		head = evidence
		for _, p := range d.Plans {
			known = known || p.ProseSHA == head
		}
		for _, v := range d.Reviews {
			known = known || v.EvidenceSHA == head
		}
	} else {
		if d.Run != nil {
			known = head != "" && head == d.Run.HeadSHA
		}
		for _, v := range d.Reviews {
			known = known || head == v.HeadSHA
		}
		for _, step := range d.History {
			known = known || (head != "" && head == step.Commit)
		}
	}
	if !known || head == "" {
		badRequest(w, "that commit is not a recorded head or evidence for this feature; reload the feature")
		return
	}
	hat := hatchery.New(project.Path)
	base := ""
	if evidence == "" {
		base = r.URL.Query().Get("base")
		if base == "" && d.Run != nil {
			// The base branch contains the feature after landing. Comparing to
			// it would erase the whole change from the historical view.
			base = d.Run.BaseSHA
		}
		if base == "" {
			base = project.BaseBranch
		}
		base, err = hat.Resolve(r.Context(), base)
		if err != nil {
			s.featureFileError(w, r, err)
			return
		}
	}
	const maxFile = 256 * 1024
	if path := r.URL.Query().Get("path"); path != "" {
		file, err := hat.LoadFile(r.Context(), base, head, path, maxFile)
		if err != nil {
			s.featureFileError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, file)
		return
	}
	var files []hatchery.ChangedFile
	if evidence == "" {
		files, err = hat.RangeFiles(r.Context(), base, head, maxFile, 30)
	} else {
		files, err = hat.ChangedFiles(r.Context(), head, maxFile, 30)
	}
	if err != nil {
		s.featureFileError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": orEmpty(files), "base": base, "head": head})
}

func (s *Server) featureFileError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, hatchery.ErrNoSuchRevision) {
		badRequest(w, fmt.Sprintf("cannot read this feature's change: %v", err))
		return
	}
	s.fail(w, r, err)
}

type featureDecisionRequest struct {
	Head string `json:"head"`
	Note string `json:"note"`
}

func (s *Server) refreshFeature(w http.ResponseWriter, r *http.Request) {
	s.changeFeature(w, r, false)
}

func (s *Server) rejectFeature(w http.ResponseWriter, r *http.Request) {
	s.changeFeature(w, r, true)
}

// These decisions both refer to the head the human was actually shown.
func (s *Server) changeFeature(w http.ResponseWriter, r *http.Request, reject bool) {
	if s.nyd == nil {
		writeError(w, http.StatusNotImplemented, "this build cannot route work")
		return
	}
	var req featureDecisionRequest
	if !decode(w, r, &req) {
		return
	}
	task, err := s.db.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if task.Kind != store.TaskKindFeature {
		badRequest(w, "that is not a feature")
		return
	}
	if reject {
		err = s.nyd.RejectFeature(r.Context(), task.ID, req.Head, req.Note)
	} else {
		err = s.nyd.RefreshFeature(r.Context(), task.ID, req.Head)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.syncArchitect(r.Context(), task.ProjectID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
