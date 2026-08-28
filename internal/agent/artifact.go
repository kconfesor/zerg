package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/store"
)

// Producing an artifact: what an agent made that a person wants to look at.
//
// Two shapes over one verb, because they are the same act from the agent's
// side and differ only in what is worth keeping. A file is copied into the
// blob store and named by its digest; a service is a port a process is
// listening on, which has no bytes at all and stops being true the moment that
// process exits.

// ArtifactArgs is the request an agent sends.
type ArtifactArgs struct {
	Kind  string `json:"kind"`            // file or service
	Path  string `json:"path,omitempty"`  // for a file
	Port  int    `json:"port,omitempty"`  // for a service
	Label string `json:"label,omitempty"` // what to call it in the cockpit
	// TaskID is optional: the card this role is holding is used when it is
	// not given, the same way a question finds its card.
	TaskID string `json:"taskId,omitempty"`
}

// dialTimeout bounds the check that a registered service is really listening.
// Loopback either answers immediately or is not there.
const dialTimeout = 300 * time.Millisecond

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.permit(w, r, CanArtifact)
	if !ok {
		return
	}
	if s.blobs == nil {
		writeError(w, http.StatusNotImplemented, "this daemon has nowhere to keep artifacts")
		return
	}
	var req ArtifactArgs
	if !decode(w, r, &req) {
		return
	}

	a := &store.Artifact{
		ProjectID: id.ProjectID,
		Role:      id.Role,
		Label:     strings.TrimSpace(req.Label),
	}

	// The card this belongs to, resolved the way a question's is: named if the
	// agent said, otherwise the one this role is holding a lease on.
	if req.TaskID != "" {
		resolved, err := s.resolveTask(r.Context(), id.ProjectID, req.TaskID)
		if err != nil {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("no task with id or name %q in this project", req.TaskID))
			return
		}
		a.TaskID = &resolved
	} else if held, err := s.db.CurrentTaskFor(r.Context(), id.ProjectID, id.Role); err == nil {
		a.TaskID = held
	}

	switch req.Kind {
	case store.ArtifactService:
		if err := s.registerService(a, req.Port); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case store.ArtifactFile, "":
		if err := s.storeFile(r.Context(), a, id, req.Path); err != nil {
			// Every failure here is the agent's to fix: a path that is not
			// there, one outside the project, a file too big to keep.
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown kind %q; it is file or service", req.Kind))
		return
	}

	saved, err := s.db.AddArtifact(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A registered service is an agent saying it got there, which is what
	// somebody watching a "working…" panel is waiting to see.
	if saved.Kind == store.ArtifactService {
		if wch := s.watcher(); wch != nil {
			wch.Served(id.ProjectID, id.Role)
		}
	}
	writeJSON(w, http.StatusCreated, saved)
}

// remember writes down what this agent learned about serving the project.
//
// The runner's memory, and the only reason a second preview is faster than the
// first. Prose, because what is worth writing down is different for a compose
// stack and a monorepo, and a schema here would be a guess about which.
func (s *Server) remember(w http.ResponseWriter, r *http.Request) {
	id, ok := s.permit(w, r, CanRemember)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.db.SaveRunNote(r.Context(), id.ProjectID, req.Note, id.Role); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "remembered"})
}

// registerService records a port, having checked something is on it.
//
// Checked rather than trusted: a typo produces a row the cockpit offers as a
// link, and the operator finds out by clicking it and waiting for a connection
// that was never going to be made.
func (s *Server) registerService(a *store.Artifact, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("a service needs the port it is listening on, 1 to 65535")
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("nothing is listening on %s: start it before registering it", addr)
	}
	conn.Close()

	a.Kind = store.ArtifactService
	a.Port = port
	if a.Label == "" {
		a.Label = fmt.Sprintf("service on %d", port)
	}
	return nil
}

// storeFile copies a file into the blob store.
func (s *Server) storeFile(ctx context.Context, a *store.Artifact, id Identity, path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("adding a file needs its path")
	}
	full, err := s.insideProject(ctx, id.ProjectID, path)
	if err != nil {
		return err
	}

	digest, mimeType, size, err := s.blobs.Put(full)
	if err != nil {
		if errors.Is(err, artifact.ErrTooLarge) {
			return err
		}
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no such file: %s", path)
		}
		return fmt.Errorf("storing %s: %w", filepath.Base(path), err)
	}

	a.Kind = store.ArtifactFile
	if strings.HasPrefix(mimeType, "image/") {
		// Its own kind rather than a check on the mime type in three places:
		// an image is the one thing the cockpit shows without being asked.
		a.Kind = store.ArtifactImage
	}
	a.SHA256, a.MIME, a.Bytes = digest, mimeType, size
	a.Name = filepath.Base(full)
	if a.Label == "" {
		a.Label = a.Name
	}
	return nil
}

// insideProject resolves a path an agent named and refuses anything outside
// the project it is working on.
//
// The agent can already run arbitrary code in its worktree, so this is not a
// wall: it is a rule about what an artifact is. Without it, `artifact add
// ~/.ssh/id_rsa` copies a key into the blob store and publishes it on the
// cockpit's HTTP surface, which has no authentication -- an agent that read a
// poisoned file could do it in one line. Symlinks are resolved before the
// check, or a link inside the tree would be the way around it.
func (s *Server) insideProject(ctx context.Context, projectID, path string) (string, error) {
	project, err := s.db.GetProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(project.Path)
	if err != nil {
		return "", fmt.Errorf("reading the project directory: %w", err)
	}

	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	full, err = filepath.EvalSymlinks(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no such file: %s", path)
		}
		return "", err
	}

	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"%s is outside the project; an artifact has to be something the work produced", path)
	}
	return full, nil
}
