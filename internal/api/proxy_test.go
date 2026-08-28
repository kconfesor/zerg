package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

// upstream is a stand-in for the dev server an agent started: it reports the
// path it was asked for, so the test can see what the proxy rewrote.
func upstream(t *testing.T) (port int, close func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=agent-set-this")
		_, _ = io.WriteString(w, "asked for "+r.URL.Path)
	})}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { srv.Close() }
}

func serviceFixture(t *testing.T) (*Proxy, *streamFixture, *store.Artifact, func()) {
	t.Helper()
	f := newFixture(t)
	port, closeUp := upstream(t)

	a, err := f.db.AddArtifact(context.Background(), &store.Artifact{
		ProjectID: f.project.ID, Role: "coder", Kind: store.ArtifactService,
		Label: "Dev server", Port: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(f.db, slog.New(slog.NewTextHandler(io.Discard, nil))), f, a, closeUp
}

// The service is reached at /<id>/..., and it does not know that. Without the
// rewrite a dev server answers 404 for its own assets.
func TestTheProxyStripsTheArtifactPrefix(t *testing.T) {
	p, _, a, closeUp := serviceFixture(t)
	defer closeUp()
	h := p.Handler()

	for _, tc := range []struct{ ask, want string }{
		{"/" + a.ID + "/", "asked for /"},
		{"/" + a.ID, "asked for /"},
		{"/" + a.ID + "/assets/main.js", "asked for /assets/main.js"},
		{"/" + a.ID + "/nested/deep/page", "asked for /nested/deep/page"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.ask, nil))
		if rec.Code != 200 || rec.Body.String() != tc.want {
			t.Errorf("%s -> %d %q, want %q", tc.ask, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// The service is somebody else's code. It has no business setting cookies on
// the origin it is being viewed from.
func TestTheProxyDropsCookiesFromTheService(t *testing.T) {
	p, _, a, closeUp := serviceFixture(t)
	defer closeUp()

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/"+a.ID+"/", nil))
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("the service set a cookie on the viewer origin: %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff on a proxied response")
	}
}

// A stopped service's port belongs to whatever binds it next. Proxying anyway
// is how a dead link quietly becomes a live one to the wrong program.
func TestAStoppedServiceIsNotProxied(t *testing.T) {
	p, f, a, closeUp := serviceFixture(t)
	defer closeUp()

	if _, err := f.db.StopServices(context.Background(), f.project.ID, ""); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/"+a.ID+"/", nil))
	if rec.Code != http.StatusGone {
		t.Errorf("status %d, want 410", rec.Code)
	}
}

// The process dies without telling anybody, which is the ordinary case.
func TestAServiceThatStoppedAnsweringSaysSo(t *testing.T) {
	p, _, a, closeUp := serviceFixture(t)
	closeUp() // it is gone, and the row does not know

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/"+a.ID+"/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not answering") {
		t.Errorf("body = %q, which does not say what happened", rec.Body.String())
	}
}

// This origin exists to be untrusted. It serves proxied services and nothing
// else: no cockpit, no API, and no link back to either.
func TestTheProxyOriginServesNothingElse(t *testing.T) {
	p, _, _, closeUp := serviceFixture(t)
	defer closeUp()
	h := p.Handler()

	for _, path := range []string{"/", "/api/projects", "/index.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404 on the service origin", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s redirected to %q; this origin must not point at the cockpit", path, loc)
		}
	}
}

// A file is not a service, and the message should say which mistake was made.
func TestAFileIsNotProxied(t *testing.T) {
	f := newFixture(t)
	a, err := f.db.AddArtifact(context.Background(), &store.Artifact{
		ProjectID: f.project.ID, Kind: store.ArtifactFile, SHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(f.db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/"+a.ID+"/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

// The link is built from the request, so it is right for whoever asked:
// loopback for a local browser, the tailnet name for a phone.
func TestAServiceLinkFollowsTheHostThatAsked(t *testing.T) {
	r := httptest.NewRequest("GET", "http://kelvins-machine.ts.net:7717/api/tasks/x/artifacts", nil)
	if got := ServiceURL(r, 45123, "ART1"); got != "http://kelvins-machine.ts.net:45123/ART1/" {
		t.Errorf("ServiceURL = %q", got)
	}

	// No proxy running: no link, rather than one that cannot work.
	if got := ServiceURL(r, 0, "ART1"); got != "" {
		t.Errorf("ServiceURL with no proxy = %q, want empty", got)
	}
}
