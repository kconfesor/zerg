package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A page you visit can resolve its own hostname to 127.0.0.1 and then post to
// this API from your browser. The tailnet cannot help — the request originates
// inside it — and agents run with permission prompts disabled, so "create a
// task" is arbitrary code execution on this machine.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	s := &Server{log: discardLogger()}
	reached := false
	h := s.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	for _, tc := range []struct {
		name, method, fetchSite, origin string
		want                            int
	}{
		// What a cockpit tab sends.
		{"same-origin write", "POST", "same-origin", "http://127.0.0.1:7717", http.StatusOK},
		{"typed url", "POST", "none", "", http.StatusOK},

		// What an attacker's page sends.
		{"cross-site write", "POST", "cross-site", "https://evil.test", http.StatusForbidden},
		{"cross-site delete", "DELETE", "cross-site", "https://evil.test", http.StatusForbidden},

		// An older browser sends no Sec-Fetch-Site, so Origin decides.
		{"mismatched origin", "POST", "", "https://evil.test", http.StatusForbidden},
		{"matching origin", "POST", "", "http://127.0.0.1:7717", http.StatusOK},

		// curl sends neither, and is not the threat this is for.
		{"no browser headers", "POST", "", "", http.StatusOK},

		// Reads are left alone: refusing them would break linking to the
		// cockpit, and what a GET leaks is authentication's problem.
		{"cross-site read", "GET", "cross-site", "https://evil.test", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:7717/api/projects", strings.NewReader("{}"))
			r.Host = "127.0.0.1:7717"
			if tc.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if tc.want == http.StatusOK && !reached {
				t.Errorf("a legitimate %s was refused (%d)", tc.name, w.Code)
			}
			if tc.want == http.StatusForbidden {
				if reached {
					t.Error("the request reached the handler")
				}
				if w.Code != http.StatusForbidden {
					t.Errorf("status %d, want 403", w.Code)
				}
			}
		})
	}
}

// A body that never ends would otherwise sit in memory until the machine
// decides which process to kill.
func TestOversizedBodiesAreCut(t *testing.T) {
	s := &Server{log: discardLogger()}
	var readErr error
	h := s.guard(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64*1024)
		for {
			if _, err := r.Body.Read(buf); err != nil {
				readErr = err
				return
			}
		}
	}))

	r := httptest.NewRequest("POST", "http://127.0.0.1:7717/api/projects",
		strings.NewReader(strings.Repeat("x", maxBody+1024)))
	r.Host = "127.0.0.1:7717"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if readErr == nil || !strings.Contains(readErr.Error(), "too large") {
		t.Errorf("read ended with %v, want a too-large error", readErr)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
