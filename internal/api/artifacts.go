package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Serving artifacts.
//
// Ordinary HTTP resources, which is §13.1's whole argument: bytes over the
// event stream would mean base64, hand-rolled chunking, no caching and no
// range requests, and would head-of-line block the live events behind a four
// megabyte screenshot. A GET already does all of that well.

// taskArtifacts lists what a card produced.
func (s *Server) taskArtifacts(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.ArtifactsForTask(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(list))
}

// artifactBytes serves a stored file.
//
// http.ServeContent rather than io.Copy: it answers range requests, handles
// If-None-Match against the ETag and writes the 304, which is what makes a
// video scrub and a reload cheap. The digest is the ETag because it is what
// the file is named by -- two artifacts with the same content have the same
// one, correctly.
func (s *Server) artifactBytes(w http.ResponseWriter, r *http.Request) {
	a, err := s.db.GetArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.blobs == nil || a.SHA256 == "" {
		badRequest(w, "this artifact has no stored bytes: it is a "+a.Kind)
		return
	}

	f, err := s.blobs.Open(a.SHA256)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The row outlived the file: retention removed the bytes, or
			// somebody cleaned the directory. Said plainly rather than as a
			// fault, because it is neither the caller's mistake nor a bug.
			writeError(w, http.StatusGone, "the bytes of this artifact are no longer stored")
			return
		}
		s.fail(w, r, err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	// Immutable: the name is the content, so this response can never become
	// wrong. A reload of a card with ten screenshots then costs nothing.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	// An agent's output is not this daemon's code. Rendered inline on the
	// cockpit's own origin, an HTML artifact would run with access to
	// everything the cockpit can reach, which is the same argument §13.4 makes
	// about proxied services. Anything that is not a picture or plain text is
	// therefore a download rather than a page, and nothing here is ever
	// sniffed into something executable.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if inlineSafe(a.MIME) {
		w.Header().Set("Content-Disposition", "inline; filename="+quoted(a.Name))
	} else {
		w.Header().Set("Content-Disposition", "attachment; filename="+quoted(a.Name))
	}

	http.ServeContent(w, r, a.Name, info.ModTime(), f)
}

// pinArtifact keeps an artifact after its task's transcript ages out.
func (s *Server) pinArtifact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.db.SetArtifactPinned(r.Context(), r.PathValue("id"), req.Pinned); err != nil {
		s.fail(w, r, err)
		return
	}
	a, err := s.db.GetArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// inlineSafe reports whether a type can be shown in the page rather than
// downloaded.
//
// A short allowlist, not a blocklist. Images and plain text render as
// themselves and cannot execute; HTML, SVG and anything scriptable are
// downloads, because an agent wrote them and the cockpit's origin is the last
// place they should run. SVG is the one that surprises people: it is a
// document format that can carry script.
func inlineSafe(mime string) bool {
	base := strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])
	switch base {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif",
		"text/plain", "text/markdown", "application/json":
		return true
	}
	return false
}

// quoted makes a filename safe to put in a header.
//
// A quote or a newline in a filename is a header injection, and an agent picks
// these names. Anything unusual is replaced rather than escaped: the name is a
// convenience for whoever saves the file, and the artifact's identity is its
// id.
func quoted(name string) string {
	if name == "" {
		name = "artifact"
	}
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r == '/' || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("%q", clean)
}
