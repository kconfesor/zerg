package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
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

// The Host check is what actually stops DNS rebinding, and it is the reason
// safe methods can stay unguarded otherwise.
//
// Rebinding points an attacker's hostname at this machine, so by the time the
// request arrives the browser calls it same-origin and sends a matching Origin.
// Both of the other checks pass. The Host does not: it is still the attacker's
// name, because that is the name the victim's browser was aimed at.
func TestRequestsAddressedToAnotherNameAreRefused(t *testing.T) {
	h, _ := newTestServer(t)

	cases := []struct {
		name  string
		host  string
		allow bool
	}{
		{"loopback address", "127.0.0.1:7717", true},
		{"loopback name", "localhost:7717", true},
		{"IPv6 loopback", "[::1]:7717", true},
		{"an attacker's name resolved here", "evil.example", false},
		{"a tailnet name this daemon does not serve", "someone-else.tailnet.ts.net", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A GET, because reading is the whole of the attack: /api/browse
			// enumerates the filesystem.
			r := httptest.NewRequest("GET", "/api/projects", nil)
			r.Host = tc.host
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			if tc.allow && rec.Code == http.StatusForbidden {
				t.Errorf("Host %q was refused; the cockpit is reached this way", tc.host)
			}
			if !tc.allow && rec.Code != http.StatusForbidden {
				t.Errorf("Host %q got %d, want 403", tc.host, rec.Code)
			}
		})
	}
}

// The tailnet name the daemon serves TLS for is how a phone reaches it, so it
// has to pass while other names under the same suffix do not.
func TestTheTailnetNameThisDaemonServesIsAllowed(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{
		DB:  db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Applied: store.Listener{
			Addr:        "100.97.75.92:7717",
			TailnetHost: "kelvins-machine.tailnet.ts.net",
		},
	}).Routes()

	for host, want := range map[string]int{
		"kelvins-machine.tailnet.ts.net:7717": http.StatusOK,
		"100.97.75.92:7717":                   http.StatusOK,
		"other-machine.tailnet.ts.net:7717":   http.StatusForbidden,
	} {
		r := httptest.NewRequest("GET", "/api/projects", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %q got %d, want %d", host, rec.Code, want)
		}
	}
}

// The discovered tailnet name is allowed without being written into the saved
// listener, which is the shape that broke the settings view.
//
// Applied is compared against the stored configuration to decide whether a
// restart is pending. A discovered value in there is a daemon reporting a
// change nobody made, permanently, since restarting rediscovers it.
func TestTheDiscoveredTailnetNameIsAllowedWithoutFakingAPendingRestart(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}

	// What a tailscale-TLS install looks like: an address, no configured name.
	saved := store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale}
	s := New(Deps{
		DB:          db,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Applied:     saved.Listener(),
		TailnetHost: "kelvins-machine.tailnet.ts.net",
	})
	h := s.Routes()

	for host, want := range map[string]int{
		"kelvins-machine.tailnet.ts.net:7717": http.StatusOK,
		"100.64.0.1:7717":                     http.StatusOK,
		"127.0.0.1:7717":                      http.StatusOK,
		"evil.example":                        http.StatusForbidden,
	} {
		r := httptest.NewRequest("GET", "/api/projects", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %q got %d, want %d", host, rec.Code, want)
		}
	}

	if s.restartNeeded(saved) {
		t.Error("a freshly started daemon says a restart is pending, which no restart can clear")
	}
}
