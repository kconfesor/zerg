package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/adapter/claudeharness"
	"github.com/kconfesor/zerg/internal/store"
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

	if len(roles) != 8 {
		t.Fatalf("library has %d roles, want the 8 built-ins", len(roles))
	}
	names := map[string]bool{}
	for _, r := range roles {
		names[r.Name] = true
	}
	for _, want := range []string{"planner", "coder", "reviewer", "cleaner",
		"architect", "hardener", "security", "docs"} {
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
	var team []store.ResolvedRole
	decodeInto(t, rec, &team)
	if len(team) != 2 || team[0].Name != "coder" || team[1].Name != "reviewer" {
		t.Fatalf("default team is wrong: %+v", team)
	}
	if !team[1].Terminal {
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

	opus := "opus"
	rec = do(t, h, "PUT", "/api/projects/"+p.ID+"/team", []store.ProjectRole{
		{TemplateID: id("planner"), Enabled: true},
		{TemplateID: id("coder"), Enabled: true, ModelOverride: &opus},
		{TemplateID: id("reviewer"), Enabled: true},
		{TemplateID: id("docs"), Enabled: false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("setTeam status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var team []store.ResolvedRole
	decodeInto(t, rec, &team)
	if len(team) != 4 {
		t.Fatalf("team has %d roles, want 4", len(team))
	}
	if team[0].Name != "planner" || team[0].Gate != store.GateApproval {
		t.Error("planner should lead and carry its approval gate")
	}
	if team[1].Model != "opus" || !team[1].Overridden {
		t.Errorf("coder override not applied or not flagged: model=%q overridden=%v",
			team[1].Model, team[1].Overridden)
	}
	// A disabled trailing role must not steal terminality.
	if team[3].Terminal {
		t.Error("a disabled role must never be terminal")
	}
	if !team[2].Terminal {
		t.Error("reviewer is the last enabled role and must be terminal")
	}
}

func TestSetTeamOnMissingProjectIs404(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "PUT", "/api/projects/NOSUCHID/team", []store.ProjectRole{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
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

func TestCockpitIsServedAtTheRoot(t *testing.T) {
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
