package api

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

// upstream is a stand-in for the dev server an agent started: it reports the
// path it was asked for, so a test can see whether anything was rewritten, and
// it sets a cookie the way anything with a login does.
func upstream(t *testing.T) (port int, close func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=agent-set-this; Path=/; HttpOnly")
		_, _ = io.WriteString(w, "asked for "+r.URL.Path)
	})}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { srv.Close() }
}

func serviceFixture(t *testing.T) (*Viewer, *streamFixture, *store.Artifact, func()) {
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
	v := NewViewer(f.db, "127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(v.CloseAll)
	return v, f, a, closeUp
}

// get fetches a path from a service's own origin.
func get(t *testing.T, port int, path string) *http.Response {
	t.Helper()
	resp, err := http.Get("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// A service keeps its own paths.
//
// It was reached at /<id>/... on one shared origin, and the prefix had to be
// stripped on the way through. That works for a static directory and fails for
// every dev server: the HTML they serve names absolute scripts like
// /src/main.ts, the browser asks the shared origin's root for it, and the page
// renders blank while the HTML itself came back 200. An origin per service is
// what makes the service's own paths already correct.
func TestAServiceIsReachedOnItsOwnPathsAndItsOwnOrigin(t *testing.T) {
	v, _, a, closeUp := serviceFixture(t)
	defer closeUp()

	port := v.PortFor(a)
	if port == 0 {
		t.Fatal("no origin was opened for the service")
	}

	for _, path := range []string{"/", "/src/main.ts", "/assets/index-a1b2.js", "/works/nightjar"} {
		resp := get(t, port, path)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "asked for "+path {
			t.Errorf("%s -> %d %q, want the service asked for %s", path, resp.StatusCode, body, path)
		}
	}

	// The same service asked for twice is the same origin, not a second one.
	if again := v.PortFor(a); again != port {
		t.Errorf("a second look opened another origin: %d then %d", port, again)
	}
}

// Two services are two origins, which is what keeps their cookies and their
// storage apart without anything being rewritten.
func TestTwoServicesGetTwoOrigins(t *testing.T) {
	v, f, a, closeUp := serviceFixture(t)
	defer closeUp()

	otherPort, closeOther := upstream(t)
	defer closeOther()
	b, err := f.db.AddArtifact(context.Background(), &store.Artifact{
		ProjectID: f.project.ID, Role: "coder", Kind: store.ArtifactService,
		Label: "The API", Port: otherPort,
	})
	if err != nil {
		t.Fatal(err)
	}

	if v.PortFor(a) == v.PortFor(b) {
		t.Error("two services landed on one origin, so one could read the other's session")
	}

	// A cookie survives, because an origin of its own is what makes that safe.
	// Dropping them made anything with a login impossible to look at.
	resp := get(t, v.PortFor(a), "/")
	resp.Body.Close()
	if got := resp.Header.Get("Set-Cookie"); !strings.Contains(got, "session=agent-set-this") {
		t.Errorf("the service's cookie did not survive: %q", got)
	}
}

// A stopped service is not given an origin: its port belongs to whatever binds
// it next, and proxying anyway is how a dead link becomes a live one to the
// wrong program.
func TestAStoppedServiceIsNotProxied(t *testing.T) {
	v, f, a, closeUp := serviceFixture(t)
	defer closeUp()

	if _, err := f.db.StopServices(context.Background(), f.project.ID, ""); err != nil {
		t.Fatal(err)
	}
	stopped, err := f.db.GetArtifact(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if port := v.PortFor(stopped); port != 0 {
		t.Errorf("a stopped service was given origin %d", port)
	}
}

// Reading a preview keeps its runner alive.
//
// The idle timer used to measure a signal nothing sent: the cockpit never
// called touch, so a preview somebody was using died thirty minutes after it
// started. A request through here is the honest signal, and the only one that
// is true from a phone or from a tab left open.
func TestReadingAServiceKeepsItsRunnerAlive(t *testing.T) {
	v, f, a, closeUp := serviceFixture(t)
	defer closeUp()

	var touched []string
	v.WithTouch(func(projectID string) { touched = append(touched, projectID) })

	get(t, v.PortFor(a), "/").Body.Close()
	if len(touched) != 1 || touched[0] != f.project.ID {
		t.Errorf("touched %v, want one report for %s", touched, f.project.ID)
	}
}

// The link is built from the host the browser used, so the same daemon answers
// a phone on a tailnet and a browser on loopback with an address each can
// reach.
func TestTheLinkFollowsWhoeverIsAsking(t *testing.T) {
	for _, tc := range []struct {
		host, want string
		tls        bool
	}{
		{host: "127.0.0.1:7717", want: "http://127.0.0.1:52000/"},
		{host: "kelvin.tail1234.ts.net:7717", want: "https://kelvin.tail1234.ts.net:52000/", tls: true},
		{host: "100.97.75.92:7717", want: "http://100.97.75.92:52000/"},
	} {
		r := httptest.NewRequest("GET", "/api/tasks/x/artifacts", nil)
		r.Host = tc.host
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := ServiceURL(r, 52000); got != tc.want {
			t.Errorf("from %s the link is %q, want %q", tc.host, got, tc.want)
		}
	}

	// No origin means no link, rather than one that cannot be reached.
	if got := ServiceURL(httptest.NewRequest("GET", "/", nil), 0); got != "" {
		t.Errorf("link without an origin: %q", got)
	}
}
