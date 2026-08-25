package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The built cockpit, compiled into the binary so `zerg up` needs nothing on
// disk beside it.
//
// The `all:` prefix is required and easy to miss: a plain //go:embed skips
// files and directories beginning with `.` or `_`, which silently drops Vite's
// dist/.vite/ manifest. The build appears to succeed and the page fails to load.
//
//go:embed all:dist
var cockpitFS embed.FS

// cockpit serves the single-page app.
//
// Anything that is not a real asset falls back to index.html so a deep link
// reloads correctly, but /api is never rewritten — a mistyped endpoint must
// return a 404 the caller can see, not an HTML page that parses as neither.
func cockpit() (http.Handler, error) {
	sub, err := fs.Sub(cockpitFS, "dist")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// This handler is mounted on "/", so it receives every API path the
		// mux did not match. Falling those through to index.html would answer
		// a mistyped endpoint with 200 and a page of HTML, which a client
		// parses as neither JSON nor an error — a wrong URL would look like a
		// malformed response for as long as it took someone to check.
		if path == "api" || strings.HasPrefix(path, "api/") {
			writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
			return
		}

		if f, err := sub.Open(path); err == nil {
			f.Close()
			// Hashed asset names make the content immutable; index.html is not
			// hashed, so it must always be revalidated or a deploy would serve
			// an old shell pointing at assets that no longer exist.
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r2)
	}), nil
}
