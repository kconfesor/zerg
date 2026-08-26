package api

import (
	"embed"
	"io"
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
// The directory holds only a committed .gitkeep in a fresh clone: the built
// cockpit is generated output and is not in git, because every asset filename
// carries a content hash, so any two branches that both built it conflicted on
// files nobody writes by hand. `all:` embeds the .gitkeep, which is what keeps
// this compiling before anyone has run ./build.sh.
//
//go:embed all:dist
var cockpitFS embed.FS

// built reports whether a cockpit was compiled in, rather than the placeholder
// that only exists to satisfy the embed.
func built(sub fs.FS) bool {
	f, err := sub.Open("index.html")
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// notBuilt answers every page with the one thing worth saying.
//
// The alternative is a 404 on the cockpit and a working API, which reads as a
// broken install rather than an unfinished one. The daemon itself is fine here;
// only the UI is missing, and one command produces it.
const notBuilt = `<!doctype html>
<meta charset="utf-8">
<title>zerg: cockpit not built</title>
<body style="font: 14px/1.6 ui-monospace, monospace; background: #16121c; color: #d7cfe0; padding: 2rem">
<h1 style="font-size: 1rem">The cockpit is not built</h1>
<p>The daemon is running and its API is up. The UI is generated rather than committed, so build it:</p>
<pre style="background: #1e1826; padding: 1rem; overflow-x: auto">./build.sh    # then restart zerg up</pre>
<p>It needs Node and pnpm; see the README for versions.</p>
</body>`

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
	haveUI := built(sub)

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

		if !haveUI {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, notBuilt)
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
