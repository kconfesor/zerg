package api

import (
	"errors"
	"net/http"

	"github.com/kconfesor/zerg/internal/hatchery"
)

// The code explorer: browse any branch, tag or commit of a project's
// repository, independent of any task, approval or feature. See
// docs/design/code-explorer.md.

// repoMaxFile bounds a single file read, the same cap the diff endpoints use
// -- see hatchery.LoadFile's callers.
const repoMaxFile = 256 * 1024

// repoRefs lists a project's branches and tags, for the ref picker.
func (s *Server) repoRefs(w http.ResponseWriter, r *http.Request) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	refs, err := hatchery.New(project.Path).Refs(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(refs))
}

// repoResolve is the one place a client-supplied ref becomes a commit sha,
// shared by repoTree and repoFile so a directory listing and the file opened
// from it answer against the same ref resolution rather than two separate
// ones a push in between could disagree on. See decision 4.
func (s *Server) repoResolve(w http.ResponseWriter, r *http.Request) (hat *hatchery.Hatchery, sha string, ok bool) {
	project, err := s.db.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return nil, "", false
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		badRequest(w, "ref is required")
		return nil, "", false
	}
	hat = hatchery.New(project.Path)
	sha, err = hat.ResolveCommit(r.Context(), ref)
	if err != nil {
		s.repoError(w, r, err)
		return nil, "", false
	}
	return hat, sha, true
}

// repoTree lists one directory's immediate children at a ref, pinned to the
// commit sha it resolved. Not recursive -- a directory is opened because a
// person clicked it, not read whole up front.
func (s *Server) repoTree(w http.ResponseWriter, r *http.Request) {
	hat, sha, ok := s.repoResolve(w, r)
	if !ok {
		return
	}
	entries, err := hat.Tree(r.Context(), sha, r.URL.Query().Get("path"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolvedSha": sha, "entries": orEmpty(entries)})
}

// repoFile reads one file's content at a ref, pinned to the commit sha it
// resolved.
func (s *Server) repoFile(w http.ResponseWriter, r *http.Request) {
	hat, sha, ok := s.repoResolve(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		badRequest(w, "path is required")
		return
	}
	blob, err := hat.Blob(r.Context(), sha, path, repoMaxFile)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolvedSha": sha, "blob": blob})
}

// repoError is the same client/server split every other git-reading handler
// in this package draws: a ref or path the repository does not have is the
// operator's to fix, not this daemon's fault.
func (s *Server) repoError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, hatchery.ErrNoSuchRevision) {
		badRequest(w, err.Error())
		return
	}
	s.fail(w, r, err)
}
