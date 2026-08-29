package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/chat"
	"github.com/kconfesor/zerg/internal/store"
)

// Attaching a file to a chat message.
//
// The bytes go to the blob store under their digest, and a row records them
// against the project with no task, which is what lets the conversation show
// the picture again after a reload -- and after the chat worktree, where the
// agent actually read it, has been removed. The same file attached twice costs
// one copy.
//
// Deliberately not the agent's own `artifact add`: that verb is for what an
// agent produced from inside a worktree it owns, and this is a person handing
// something in from outside. They share a store and nothing else.

// attachToChat takes one uploaded file and records it.
func (s *Server) attachToChat(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		writeError(w, http.StatusServiceUnavailable, "this build cannot store files")
		return
	}
	projectID := r.PathValue("id")
	if _, err := s.db.GetProject(r.Context(), projectID); err != nil {
		s.fail(w, r, err)
		return
	}

	// The multipart reader is given the same ceiling as the body, so a file
	// over the limit fails as a size rather than as a broken read halfway in.
	file, header, err := r.FormFile("file")
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			badRequest(w, "attach a file under the form field \"file\"")
			return
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			badRequest(w, fmt.Sprintf("that file is over the %d MB limit for an attachment", maxUpload>>20))
			return
		}
		badRequest(w, "could not read the upload: "+err.Error())
		return
	}
	defer file.Close()

	// Through a temporary file rather than memory: the store hashes what it
	// copies, and a phone photograph held in RAM per upload is a cost with no
	// reason behind it.
	tmp, err := os.CreateTemp("", "zerg-upload-*")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			badRequest(w, fmt.Sprintf("that file is over the %d MB limit for an attachment", maxUpload>>20))
			return
		}
		s.fail(w, r, err)
		return
	}
	if err := tmp.Close(); err != nil {
		s.fail(w, r, err)
		return
	}

	digest, mimeType, size, err := s.blobs.Put(tmp.Name())
	if err != nil {
		if errors.Is(err, artifact.ErrTooLarge) {
			badRequest(w, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}

	name := safeName(header.Filename)
	a := &store.Artifact{
		ProjectID: projectID,
		// The person, not an agent. The transcript says who attached it, and
		// the retention rules treat a conversation's files as the conversation.
		Role:   chat.Operator,
		Kind:   store.ArtifactFile,
		Label:  name,
		Name:   name,
		SHA256: digest,
		MIME:   mimeType,
		Bytes:  size,
		Owner:  store.OwnerDaemon,
	}
	if strings.HasPrefix(mimeType, "image/") {
		// Its own kind, because an image is the one thing shown without being
		// asked -- in the conversation as well as on a card.
		a.Kind = store.ArtifactImage
	}
	made, err := s.db.AddArtifact(r.Context(), a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, made)
}

// safeName reduces whatever the browser sent to a plain file name.
//
// The name reaches a path inside the chat worktree, so a browser that sends
// "../../.ssh/authorized_keys" -- or a person who named a file that -- must not
// be able to choose where it lands.
func safeName(raw string) string {
	name := filepath.Base(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, string(filepath.Separator), "-")
	// Leading dots hide the file from the agent's own listing as surely as
	// from a person's, and "." and ".." are not names at all.
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "attachment"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}
