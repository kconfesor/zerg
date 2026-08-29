package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"fmt"
	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/adapter/claudeharness"
	"github.com/kconfesor/zerg/internal/store"
	"time"
)

func newTestServer(t *testing.T) (http.Handler, *store.DB) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := adapter.NewRegistry()
	reg.Register(claudeharness.New())
	return New(Deps{DB: db, Log: log, Registry: reg}).Routes(), db
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	// What a cockpit tab sends. httptest defaults to example.com, which the
	// daemon now refuses: a Host it does not serve is what DNS rebinding looks
	// like, so every request here carries a real one.
	req.Host = "127.0.0.1:7717"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
}

func TestHealth(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestListRolesReturnsTheSeededLibrary(t *testing.T) {
	h, _ := newTestServer(t)

	rec := do(t, h, "GET", "/api/roles", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var roles []store.RoleTemplate
	decodeInto(t, rec, &roles)

	// Nine pipeline roles and the runner, which is a role like any other so
	// that its harness, model, thinking and prompt are editable here rather
	// than being the one agent nobody can configure. It is not in the nine
	// because it is never part of a team.
	if len(roles) != 10 {
		t.Fatalf("library has %d roles, want nine built-ins and the runner", len(roles))
	}
	names := map[string]bool{}
	for _, r := range roles {
		names[r.Name] = true
	}
	for _, want := range []string{"planner", "coder", "reviewer", "debugger", "cleaner",
		"architect", "hardener", "security", "docs", "runner"} {
		if !names[want] {
			t.Errorf("built-in %q is missing from the library", want)
		}
	}
}

func TestCreateRoleRejectsInvalidNameWith400(t *testing.T) {
	h, _ := newTestServer(t)

	rec := do(t, h, "POST", "/api/roles", map[string]any{
		"name": "../escape", "harness": "claude", "receive": "task", "gate": "none",
	})
	// A bad name is the caller's mistake, so it must read as one rather than
	// as a server fault.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var body map[string]string
	decodeInto(t, rec, &body)
	if body["error"] == "" {
		t.Error("a 400 must explain what was wrong")
	}
}

func TestDuplicateRoleNameIsA400NotA500(t *testing.T) {
	h, _ := newTestServer(t)

	// "coder" is already in the seeded library.
	rec := do(t, h, "POST", "/api/roles", map[string]any{
		"name": "coder", "harness": "claude", "receive": "task", "gate": "none",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a taken name: %s", rec.Code, rec.Body)
	}
}

func TestCreateRoleCannotClaimBuiltin(t *testing.T) {
	h, _ := newTestServer(t)

	rec := do(t, h, "POST", "/api/roles", map[string]any{
		"name": "custom", "harness": "claude", "receive": "task", "gate": "none",
		"builtin": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var created store.RoleTemplate
	decodeInto(t, rec, &created)
	if created.Builtin {
		t.Error("a client managed to mark its own role as a built-in")
	}
}

func TestGetMissingRoleIs404(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/roles/NOSUCHID", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	h, _ := newTestServer(t)
	// A typo in a field name should fail loudly rather than be silently dropped,
	// which is how a UI ends up thinking it saved something it did not.
	rec := do(t, h, "POST", "/api/roles", map[string]any{
		"name": "custom", "harness": "claude", "receive": "task", "gate": "none",
		"modle": "opus",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field: %s", rec.Code, rec.Body)
	}
}

func TestProjectLifecycleAndDefaultTeam(t *testing.T) {
	h, _ := newTestServer(t)
	dir := t.TempDir()

	rec := do(t, h, "POST", "/api/projects", map[string]any{"path": dir})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var p store.Project
	decodeInto(t, rec, &p)

	// A new project must arrive usable, not empty.
	rec = do(t, h, "GET", "/api/projects/"+p.ID+"/team", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("team status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var team store.ProjectTeam
	decodeInto(t, rec, &team)
	if team.PresetID == nil || *team.PresetID != store.DefaultTeamPresetID {
		t.Fatalf("new project did not inherit the default preset: %+v", team)
	}
	if len(team.Roles) != 2 || team.Roles[0].Name != "coder" || team.Roles[1].Name != "reviewer" {
		t.Fatalf("default team is wrong: %+v", team.Roles)
	}
	if !team.Roles[1].Terminal {
		t.Error("the last enabled role must be terminal")
	}

	rec = do(t, h, "DELETE", "/api/projects/"+p.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/projects/"+p.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete, status = %d, want 404", rec.Code)
	}
}

func TestSetTeamReordersAndOverrides(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()

	rec := do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()})
	var p store.Project
	decodeInto(t, rec, &p)

	id := func(name string) string {
		tpl, err := db.GetTemplateByName(ctx, name)
		if err != nil {
			t.Fatalf("GetTemplateByName(%q): %v", name, err)
		}
		return tpl.ID
	}

	// The pipeline is a team's, so a project that wants its own gets one that
	// belongs to it. The project layer is what it overrides on top.
	opus := "opus"
	rec = do(t, h, "POST", "/api/team-presets", map[string]any{
		"name": "Its own team", "projectId": p.ID,
		"roles": []store.TeamPresetRole{
			{TemplateID: id("planner"), Position: 0, Enabled: true},
			{TemplateID: id("coder"), Position: 1, Enabled: true},
			{TemplateID: id("reviewer"), Position: 2, Enabled: true},
			{TemplateID: id("docs"), Position: 3, Enabled: false},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating the project's team: %d %s", rec.Code, rec.Body)
	}
	var own store.TeamPreset
	decodeInto(t, rec, &own)

	rec = do(t, h, "PUT", "/api/projects/"+p.ID+"/team", map[string]any{
		"presetId": own.ID,
		"roles": []store.ProjectRole{
			{TemplateID: id("coder"), RoleOverrides: store.RoleOverrides{ModelOverride: &opus}},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setTeam status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var team store.ProjectTeam
	decodeInto(t, rec, &team)
	if len(team.Roles) != 4 {
		t.Fatalf("team has %d roles, want 4", len(team.Roles))
	}
	if team.Roles[0].Name != "planner" || team.Roles[0].Gate != store.GateApproval {
		t.Error("planner should lead and carry its approval gate")
	}
	if team.Roles[1].Model != "opus" || !team.Roles[1].Overridden {
		t.Errorf("coder override not applied or not flagged: model=%q overridden=%v",
			team.Roles[1].Model, team.Roles[1].Overridden)
	}
	// A disabled trailing role must not steal terminality.
	if team.Roles[3].Terminal {
		t.Error("a disabled role must never be terminal")
	}
	if !team.Roles[2].Terminal {
		t.Error("reviewer is the last enabled role and must be terminal")
	}
}

func TestSetTeamOnMissingProjectIs404(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "PUT", "/api/projects/NOSUCHID/team", map[string]any{"roles": []store.ProjectRole{}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestTeamPresetCRUDAndProjectSelection(t *testing.T) {
	h, db := newTestServer(t)
	coder, err := db.GetTemplateByName(context.Background(), "coder")
	if err != nil {
		t.Fatal(err)
	}
	prompt := "preset prompt"
	rec := do(t, h, "POST", "/api/team-presets", store.TeamPreset{
		Name: "Docs API", Builtin: true,
		Roles: []store.TeamPresetRole{{TemplateID: coder.ID, Enabled: true, RoleOverrides: store.RoleOverrides{PromptOverride: &prompt}}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create preset: %d %s", rec.Code, rec.Body)
	}
	var preset store.TeamPreset
	decodeInto(t, rec, &preset)
	if preset.Builtin {
		t.Error("client-created preset claimed builtin")
	}

	rec = do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()})
	var p store.Project
	decodeInto(t, rec, &p)
	rec = do(t, h, "PUT", "/api/projects/"+p.ID+"/team", map[string]any{
		"presetId": preset.ID, "roles": []store.ProjectRole{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("select preset: %d %s", rec.Code, rec.Body)
	}
	var team store.ProjectTeam
	decodeInto(t, rec, &team)
	if team.PresetID == nil || *team.PresetID != preset.ID || team.Roles[0].Prompt != prompt {
		t.Fatalf("selected preset did not resolve: %+v", team)
	}
	if rec := do(t, h, "DELETE", "/api/team-presets/"+preset.ID, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("in-use preset delete = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestIntegrationUpdatePersistsDraftPR(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()})
	var p store.Project
	decodeInto(t, rec, &p)
	rec = do(t, h, "PUT", "/api/projects/"+p.ID+"/integration", map[string]any{"integration": "pr", "prDraft": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("integration update: %d %s", rec.Code, rec.Body)
	}
	decodeInto(t, rec, &p)
	if p.Integration != store.IntegratePR || !p.PRDraft {
		t.Fatalf("draft PR setting not returned: %+v", p)
	}
}

func TestSharedInstructionsRoundTrip(t *testing.T) {
	h, _ := newTestServer(t)

	rec := do(t, h, "GET", "/api/settings/shared-instructions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body instructionsBody
	decodeInto(t, rec, &body)
	if body.Text == "" {
		t.Fatal("shared instructions should be seeded, not empty")
	}

	rec = do(t, h, "PUT", "/api/settings/shared-instructions",
		instructionsBody{Text: "my own protocol notes"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200: %s", rec.Code, rec.Body)
	}

	rec = do(t, h, "GET", "/api/settings/shared-instructions", nil)
	decodeInto(t, rec, &body)
	if body.Text != "my own protocol notes" {
		t.Errorf("text = %q, want the value just written", body.Text)
	}
}

// An empty collection must encode as [] rather than null, so the cockpit never
// has to special-case "no rows yet".
func TestEmptyCollectionsEncodeAsArrays(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/projects", nil)
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("empty project list encoded as %q, want []", got)
	}
}

// ── cockpit ───────────────────────────────────────────────────────────────

// needCockpit skips a test that requires the built UI.
//
// The cockpit is generated rather than committed, so a clone that has not run
// ./build.sh embeds only the placeholder. Skipping is honest: these tests are
// about serving a page that does not exist here yet, and failing would tell a
// contributor their checkout is broken when it is merely unbuilt. CI builds the
// cockpit and runs them for real.
func needCockpit(t *testing.T) {
	t.Helper()
	sub, err := fs.Sub(cockpitFS, "dist")
	if err != nil {
		t.Fatal(err)
	}
	if !built(sub) {
		t.Skip("cockpit not built; run ./build.sh to exercise this")
	}
}

func TestCockpitIsServedAtTheRoot(t *testing.T) {
	needCockpit(t)
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("the root did not serve the app shell:\n%.200s", rec.Body.String())
	}
}

// A deep link must reload, so unknown paths fall through to the app shell.
func TestDeepLinksFallBackToTheAppShell(t *testing.T) {
	needCockpit(t)
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/projects/anything", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Error("a deep link did not reach the app shell")
	}
}

// But an API path must never fall through. Answering a mistyped endpoint with
// 200 and a page of HTML gives a client something that parses as neither JSON
// nor an error, so a wrong URL looks like a malformed response.
func TestUnknownApiPathIs404NotHtml(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %.120s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("an API path was answered with HTML")
	}
	var body map[string]string
	decodeInto(t, rec, &body)
	if body["error"] == "" {
		t.Error("a 404 on an API path must explain itself in JSON")
	}
}

// Hashed assets are immutable; the shell must always revalidate, or a deploy
// serves an old index.html pointing at assets that no longer exist.
func TestAssetCachingHeaders(t *testing.T) {
	needCockpit(t)
	h, _ := newTestServer(t)

	shell := do(t, h, "GET", "/", nil)
	if got := shell.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", got)
	}

	m := regexp.MustCompile(`/assets/[^"]+\.js`).FindString(shell.Body.String())
	if m == "" {
		t.Fatal("no hashed asset found in the shell")
	}
	asset := do(t, h, "GET", m, nil)
	if asset.Code != http.StatusOK {
		t.Fatalf("asset %s returned %d", m, asset.Code)
	}
	if !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", asset.Header().Get("Cache-Control"))
	}
}

// A project's mark is read out of the project, which means this endpoint opens
// a file in a directory the operator pointed at, over a cockpit with no
// authentication. What it will not open matters more than what it will.
func TestProjectIconStaysInsideTheProject(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "logo.png"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The path that is genuinely inside resolves.
	if _, err := resolveInside(root, "logo.png"); err != nil {
		t.Errorf("a file in the project was refused: %v", err)
	}

	for _, rel := range []string{
		"../secret.png",
		"assets/../../secret.png",
		"/etc/passwd",
		"escape.png",   // a symlink out of the tree
		"logo.txt",     // not an image
		"logo.png.txt", // nor is a name that ends in one
		"",             // nothing set
	} {
		if _, err := resolveInside(root, rel); err == nil {
			t.Errorf("resolveInside accepted %q", rel)
		}
	}
}

// The scan finds what a repository actually carries, and orders it so the
// first thing offered is the one most likely wanted.
func TestFindIconsPrefersALogoOverAGeneratedFavicon(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"favicon.ico",
		"public/logo.svg",
		"public/apple-touch-icon.png",
		"public/screenshot.png", // not icon-shaped, by name
		"README.md",             // not an image
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := findIcons(root)
	var paths []string
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	want := []string{"public/logo.svg", "favicon.ico", "public/apple-touch-icon.png"}
	if len(paths) != len(want) {
		t.Fatalf("found %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("position %d is %s, want %s (order: %v)", i, paths[i], want[i], paths)
		}
	}
}

// The walk has to reach where marks actually live, and refuse to pay for the
// directories that make repositories large.
//
// The first version was a fixed list of well-known directories and found
// nothing in either real project it was pointed at: one keeps its marks in
// assets/logos/, the other in frontend/<app>/public/.
func TestFindIconsReachesWhereMarksActuallyLive(t *testing.T) {
	root := t.TempDir()
	write := func(p string) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Named nothing like an icon, but in a directory that holds only marks.
	write("assets/logos/monogram_teal.svg")
	// Three directories down, which no fixed list would have guessed.
	write("frontend/admin-portal/public/favicon.svg")
	// Build output: a copy of a file that also exists in source.
	write("frontend/admin-portal/dist/favicon.svg")
	// Expensive and never interesting.
	write("node_modules/some-pkg/logo.png")
	// zerg's own worktrees, which would otherwise repeat every mark per role.
	write(".worktrees/coder/assets/logos/monogram_teal.svg")

	var paths []string
	for _, c := range findIcons(root) {
		paths = append(paths, c.Path)
	}

	want := []string{"assets/logos/monogram_teal.svg", "frontend/admin-portal/public/favicon.svg"}
	if len(paths) != len(want) {
		t.Fatalf("found %v, want exactly %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("position %d is %s, want %s", i, paths[i], want[i])
		}
	}
}

// Without a built cockpit the daemon still runs, and every page says why there
// is no UI. A 404 over a working API reads as a broken install rather than an
// unfinished one, which is the wrong thing to tell someone who has just cloned
// the repository.
func TestUnbuiltCockpitExplainsItself(t *testing.T) {
	sub, err := fs.Sub(cockpitFS, "dist")
	if err != nil {
		t.Fatal(err)
	}
	if built(sub) {
		t.Skip("cockpit is built; this is the fresh-clone case")
	}

	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "./build.sh") {
		t.Errorf("the page does not say how to build it:\n%.200s", rec.Body.String())
	}
	// The API is unaffected, which is the whole point of saying 503 on pages
	// rather than refusing to start.
	if rec := do(t, h, "GET", "/api/projects", nil); rec.Code != http.StatusOK {
		t.Errorf("api status = %d, want 200; only the UI is missing", rec.Code)
	}
}

// An unknown API path is answered here, not handed to whatever is serving the
// cockpit.
//
// With the dev server mounted, handing it on was a loop: Vite's config proxies
// /api back to this daemon, so a wrong URL bounced between the two. The bug is
// invisible with the embedded cockpit, which is why the test mounts a UI
// handler that records being reached.
func TestUnknownApiPathsNeverReachTheCockpit(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}

	var uiHits []string
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiHits = append(uiHits, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	h := New(Deps{
		DB:  db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		UI:  ui,
	}).Routes()

	for _, path := range []string{"/api", "/api/", "/api/nope", "/api/projects/x/not-a-thing"} {
		rec := do(t, h, "GET", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
			t.Errorf("GET %s content-type = %q, want JSON: an HTML answer parses as neither", path, ct)
		}
	}
	if len(uiHits) != 0 {
		t.Errorf("the cockpit was asked for %v; unknown API paths must not reach it", uiHits)
	}

	// And the UI still gets everything else, including deep links.
	if rec := do(t, h, "GET", "/board", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /board = %d, want the cockpit to answer", rec.Code)
	}
	if len(uiHits) != 1 || uiHits[0] != "/board" {
		t.Errorf("cockpit saw %v, want [/board]", uiHits)
	}
}

// A team belonging to one project stays out of another project's picker, and
// out of its team assignment, whatever is posted at the daemon.
func TestATeamCanBelongToOneProject(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()

	project := func() store.Project {
		var p store.Project
		decodeInto(t, do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()}), &p)
		return p
	}
	x, y := project(), project()
	coder, err := db.GetTemplateByName(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "POST", "/api/team-presets", map[string]any{
		"name":      "X only",
		"projectId": x.ID,
		"roles":     []map[string]any{{"templateId": coder.ID, "position": 0, "enabled": true}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a project's team: %d %s", rec.Code, rec.Body)
	}
	var created store.TeamPreset
	decodeInto(t, rec, &created)
	if created.ProjectID == nil || *created.ProjectID != x.ID {
		t.Fatalf("team came back owned by %v, want %s", created.ProjectID, x.ID)
	}

	// The teams each project is offered.
	var forX, forY []store.TeamPreset
	decodeInto(t, do(t, h, "GET", "/api/team-presets?project="+x.ID, nil), &forX)
	decodeInto(t, do(t, h, "GET", "/api/team-presets?project="+y.ID, nil), &forY)
	if !slices.ContainsFunc(forX, func(p store.TeamPreset) bool { return p.ID == created.ID }) {
		t.Error("a project is not offered its own team")
	}
	if slices.ContainsFunc(forY, func(p store.TeamPreset) bool { return p.ID == created.ID }) {
		t.Error("another project's team is offered here")
	}
	if !slices.ContainsFunc(forY, func(p store.TeamPreset) bool { return p.Builtin }) {
		t.Error("the shared built-in team is missing from a project's picker")
	}

	// And the assignment refuses it, so the owner is not merely a filter on a
	// list: anything that posts the id straight at the daemon is refused too.
	assign := map[string]any{"presetId": created.ID, "roles": []any{}}
	rec = do(t, h, "PUT", "/api/projects/"+y.ID+"/team", assign)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("putting Y on X's team: %d %s, want 400", rec.Code, rec.Body)
	}
	rec = do(t, h, "PUT", "/api/projects/"+x.ID+"/team", assign)
	if rec.Code != http.StatusOK {
		t.Errorf("putting X on its own team: %d %s", rec.Code, rec.Body)
	}
}

// A step's transcript is the window the trail gives it, not the whole card.
func TestTaskEventsReadOneStepOfTheTrail(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()

	var p store.Project
	decodeInto(t, do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()}), &p)
	task, err := db.CreateTask(ctx, p.ID, "Factorial", "body", "")
	if err != nil {
		t.Fatal(err)
	}

	at := func(min int) time.Time { return time.Date(2026, 1, 1, 9, min, 0, 0, time.UTC) }
	for _, e := range []struct {
		role   string
		minute int
		text   string
	}{
		{"coder", 2, "reading the parser"},
		{"coder", 8, "running the tests"},
		{"reviewer", 20, "checking the diff"},
		{"coder", 40, "second lap"},
	} {
		if err := db.RecordEvent(ctx, &store.Event{
			ID: store.NewID(), ProjectID: p.ID, TaskID: &task.ID,
			Role: e.role, Kind: "message", At: at(e.minute), Text: e.text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The coder's first step: its own role, its own window. Not the reviewer's
	// work, and not the lap the coder took later.
	rec := do(t, h, "GET", fmt.Sprintf("/api/tasks/%s/events?role=coder&from=%s&until=%s",
		task.ID, at(0).Format(time.RFC3339Nano), at(10).Format(time.RFC3339Nano)), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var slice struct {
		Events    []store.Event `json:"events"`
		Truncated bool          `json:"truncated"`
	}
	decodeInto(t, rec, &slice)
	if len(slice.Events) != 2 {
		t.Fatalf("got %d events, want the two inside the step: %+v", len(slice.Events), slice.Events)
	}
	if slice.Events[0].Text != "reading the parser" || slice.Events[1].Text != "running the tests" {
		t.Errorf("a step is read from where it started: %q then %q", slice.Events[0].Text, slice.Events[1].Text)
	}
	if slice.Truncated {
		t.Error("two events came back marked truncated")
	}

	// A step longer than the page says so. A transcript cut silently reads
	// exactly like a step that stopped there.
	rec = do(t, h, "GET", fmt.Sprintf("/api/tasks/%s/events?role=coder&limit=1&from=%s&until=%s",
		task.ID, at(0).Format(time.RFC3339Nano), at(10).Format(time.RFC3339Nano)), nil)
	decodeInto(t, rec, &slice)
	if len(slice.Events) != 1 || !slice.Truncated {
		t.Errorf("a cut transcript came back as %d events, truncated=%v", len(slice.Events), slice.Truncated)
	}

	// A window that has aged out is an ordinary answer, not an error: events
	// are the tier that gets swept (ARCHITECTURE §12.1).
	rec = do(t, h, "GET", fmt.Sprintf("/api/tasks/%s/events?role=docs&from=%s&until=%s",
		task.ID, at(0).Format(time.RFC3339Nano), at(10).Format(time.RFC3339Nano)), nil)
	decodeInto(t, rec, &slice)
	if rec.Code != http.StatusOK || len(slice.Events) != 0 {
		t.Errorf("an empty window answered %d %s, want 200 and no events", rec.Code, rec.Body)
	}

	if rec := do(t, h, "GET", "/api/tasks/"+task.ID+"/events?from=yesterday", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("an unreadable time answered %d, want 400 naming it", rec.Code)
	}
}

// The reader asks; the agent answers into the thread; nothing is blocked by it.
//
// The point of this feature is a person reviewing a change with help reading
// it, not an agent reviewing it for them. So a question opens a thread that
// holds nothing at the gate, and what turns an answer into an obligation is the
// reader raising it.
func TestAskingAboutAHunkDoesNotHoldTheGate(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()

	var p store.Project
	decodeInto(t, do(t, h, "POST", "/api/projects", map[string]any{"path": t.TempDir()}), &p)
	task, err := db.CreateTask(ctx, p.ID, "Postfix factorial", "add it", "")
	if err != nil {
		t.Fatal(err)
	}

	// A remark holds the gate.
	remark, err := db.OpenReviewThread(ctx, &store.ReviewThread{
		ProjectID: p.ID, TaskID: task.ID, File: "parse.rs", Line: 3,
	}, store.OperatorRole, "this loops forever")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := db.OpenReviewThreads(ctx, task.ID); n != 1 {
		t.Fatalf("a remark does not hold the gate: %d", n)
	}

	// A question does not, however many are open. Asking should not cost the
	// reader a click to dismiss, or they stop asking.
	for i := 0; i < 3; i++ {
		if _, err := db.OpenReviewThread(ctx, &store.ReviewThread{
			ProjectID: p.ID, TaskID: task.ID, File: "parse.rs", Line: 3,
			Kind: store.ThreadQuestion,
		}, store.OperatorRole, "why is this recursive?"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := db.OpenReviewThreads(ctx, task.ID); n != 1 {
		t.Errorf("questions are holding the gate: %d threads counted, want the one remark", n)
	}

	// Raising one is the reader deciding that what they learned matters.
	var threads []store.ReviewThread
	decodeInto(t, do(t, h, "GET", "/api/tasks/"+task.ID+"/review", nil), &threads)
	var question store.ReviewThread
	for _, th := range threads {
		if th.Kind == store.ThreadQuestion {
			question = th
			break
		}
	}
	if question.ID == "" {
		t.Fatal("no question thread came back")
	}
	rec := do(t, h, "POST", "/api/review-threads/"+question.ID+"/raise", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("raising: %d %s", rec.Code, rec.Body)
	}
	var raised store.ReviewThread
	decodeInto(t, rec, &raised)
	if raised.Kind != store.ThreadRemark || raised.State != store.ThreadOpen {
		t.Errorf("raised thread is %+v, want an open remark", raised)
	}
	if n, _ := db.OpenReviewThreads(ctx, task.ID); n != 2 {
		t.Errorf("open remarks = %d, want the original and the raised one", n)
	}

	// And a build with no agent says so rather than failing obscurely.
	rec = do(t, h, "POST", "/api/tasks/"+task.ID+"/review/ask", map[string]any{
		"file": "parse.rs", "line": 3, "question": "why?",
	})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("asking with no chat agent gave %d, want 501: %s", rec.Code, rec.Body)
	}
	_ = remark
}

// The question the agent is given is about the code the reader is looking at,
// and asks it to answer rather than to review.
func TestTheAskPromptCarriesTheHunkAndAsksForAnAnswer(t *testing.T) {
	prompt := askPrompt("main", "abc123", "src/parse.rs", 41,
		"+    while peek(s) == Some('!') {\n", "why is this a while and not an if?")

	for _, want := range []string{
		"src/parse.rs", "around line 41", "main..abc123",
		"while peek(s) == Some('!')", "why is this a while and not an if?",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, prompt)
		}
	}
	// The reviewer is the person. An agent that volunteers a verdict is
	// answering a question nobody asked.
	for _, want := range []string{"do not give a verdict", "leave the decision to"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not say %q", want)
		}
	}
}
