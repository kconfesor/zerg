package devui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// checkout writes the two files that identify a zerg source tree, so a test can
// say what Find is supposed to recognise without carrying a fixture directory.
func checkout(t *testing.T, module, pkgName string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module "+module+"\n\ngo 1.27.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "package.json"),
		[]byte(`{"name": "`+pkgName+`", "private": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// What Find decides gets `pnpm install` and `vite` run against it, and both
// execute code out of the directory they are given: install can run lifecycle
// scripts, vite evaluates its config. So "a web/package.json is nearby" is not
// good enough, and this is the test that says so.
func TestFindOnlyAcceptsThisProject(t *testing.T) {
	cases := []struct {
		name    string
		module  string
		pkg     string
		wantOK  bool
		because string
	}{
		{"this project", module, cockpitPkg, true, ""},
		{"somebody else's repository with a web/", "example.com/other", "their-frontend", false,
			"running zerg up in an unrelated project must not execute it"},
		{"our module name, their cockpit", module, "their-frontend", false,
			"the cockpit is identified by its package name, not by being called web/"},
		{"their module, our cockpit name", "example.com/other", cockpitPkg, false,
			"a package.json is easy to copy; the module has to agree"},
		{"a module whose name starts with ours", module + "-other", cockpitPkg, false,
			"the module directive is parsed, not searched for: a prefix is a different project"},
		{"ours, with the name commented out", "example.com/x // " + module, cockpitPkg, false,
			"a mention in a comment is not a declaration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := checkout(t, tc.module, tc.pkg)
			t.Chdir(dir)

			web, err := Find()
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Find: %v, want the cockpit at %s/web", err, dir)
				}
				if filepath.Base(web) != "web" {
					t.Errorf("Find = %q, want the web directory", web)
				}
				return
			}
			if !errors.Is(err, ErrNoSources) {
				t.Errorf("Find = %q, %v; want ErrNoSources, because %s", web, err, tc.because)
			}
		})
	}
}

// Find looks upward so it works from a subdirectory, and stops before it
// reaches directories that have nothing to do with this project.
func TestFindLooksUpwardButNotForever(t *testing.T) {
	dir := checkout(t, module, cockpitPkg)
	deep := filepath.Join(dir, "cmd", "zerg")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)
	if _, err := Find(); err != nil {
		t.Errorf("Find from cmd/zerg: %v, want the cockpit two levels up", err)
	}

	// Nothing above a temporary directory is a zerg checkout.
	t.Chdir(t.TempDir())
	if web, err := Find(); !errors.Is(err, ErrNoSources) {
		t.Errorf("Find = %q, %v; want ErrNoSources outside a checkout", web, err)
	}
}

func TestWaitReturnsWhenTheServerAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := wait(context.Background(), srv.URL, make(chan struct{}), 5*time.Second); err != nil {
		t.Errorf("wait: %v, want it to see a server that is up", err)
	}
}

// A dev server that never answers must fail the daemon's startup rather than
// leaving it proxying to a closed port.
func TestWaitGivesUp(t *testing.T) {
	// Port 0 is never listening, and the loop should not hang on it.
	err := wait(context.Background(), "http://127.0.0.1:1/", make(chan struct{}), 300*time.Millisecond)
	if err == nil {
		t.Fatal("wait succeeded against nothing")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want it to say the server never answered", err)
	}
}

// The common failure is not a slow start but a dead one: a missing dependency,
// a port taken, a config that throws. Waiting the full timeout for a process
// that has already exited wastes a minute and a half and says the wrong thing.
func TestWaitStopsWhenTheProcessDies(t *testing.T) {
	done := make(chan struct{})
	close(done)

	start := time.Now()
	err := wait(context.Background(), "http://127.0.0.1:1/", done, 30*time.Second)
	if err == nil {
		t.Fatal("wait succeeded although the process had exited")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("err = %v, want it to say the server exited", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s for a process that was already gone", elapsed)
	}
}

func TestProxyForwardsToTheDevServer(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<div id=\"app\"></div>"))
	}))
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	newProxy(target).ServeHTTP(rec, httptest.NewRequest("GET", "/@vite/client", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(got) != 1 || got[0] != "/@vite/client" {
		t.Errorf("dev server saw %v, want [/@vite/client]", got)
	}
}

// A dev server that dies mid-session leaves the daemon fronting nothing. The
// default reverse-proxy answer is an empty 502, which reads as a fault in the
// daemon rather than in the thing behind it.
func TestProxySaysWhenTheDevServerIsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()

	rec := httptest.NewRecorder()
	newProxy(target).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dev server") {
		t.Errorf("body = %q, want it to name what is not responding", rec.Body.String())
	}
}

func TestModuleOfReadsTheDirective(t *testing.T) {
	cases := map[string]string{
		"module " + module + "\n\ngo 1.27.0\n":             module,
		"// module example.com/x\nmodule " + module + "\n": module,
		"module " + module + " // the real one\n":          module,
		"\tmodule " + module + "\n":                        module,
		"module " + module + "-other\n":                    module + "-other",
		"require example.com/x v1.0.0\n":                   "",
		"":                                                 "",
	}
	for gomod, want := range cases {
		if got := moduleOf(gomod); got != want {
			t.Errorf("moduleOf(%q) = %q, want %q", gomod, got, want)
		}
	}
}
