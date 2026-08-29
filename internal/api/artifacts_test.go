package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/store"
)

// served builds a server with a blob store and one file in it, and returns the
// artifact it recorded.
func served(t *testing.T, name, body string) (http.Handler, *store.Artifact, *artifact.Store, *streamFixture) {
	t.Helper()
	f := newFixture(t)

	blobs := artifact.New(filepath.Join(t.TempDir(), "artifacts"))
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, mimeType, size, err := blobs.Put(src)
	if err != nil {
		t.Fatal(err)
	}

	a, err := f.db.AddArtifact(context.Background(), &store.Artifact{
		ProjectID: f.project.ID, Role: "coder", Kind: store.ArtifactFile,
		Label: name, SHA256: digest, MIME: mimeType, Bytes: size, Name: name,
	})
	if err != nil {
		t.Fatal(err)
	}

	deps := f.deps
	deps.Blobs = blobs
	return New(deps).Routes(), a, blobs, f
}

// The bytes come back with what a browser needs to cache them and to scrub
// through them, which is the whole reason artifacts are HTTP resources rather
// than event-stream payloads.
func TestArtifactBytesAreCacheableAndRangeable(t *testing.T) {
	body := strings.Repeat("abcdefghij", 100)
	h, a, _, _ := served(t, "report.txt", body)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil))
	if rec.Code != 200 || rec.Body.String() != body {
		t.Fatalf("status %d, %d bytes", rec.Code, rec.Body.Len())
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+a.SHA256+`"` {
		t.Errorf("ETag = %q, want the digest", etag)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q; content-addressed bytes can never go stale",
			rec.Header().Get("Cache-Control"))
	}

	// A second load costs nothing.
	again := httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil)
	again.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, again)
	if rec.Code != http.StatusNotModified {
		t.Errorf("a repeat request returned %d, want 304", rec.Code)
	}

	// A range, which is what makes a video scrub instead of restart.
	ranged := httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil)
	ranged.Header.Set("Range", "bytes=10-19")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, ranged)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != body[10:20] {
		t.Errorf("range request returned %d %q", rec.Code, rec.Body.String())
	}
}

// An agent wrote these bytes. The cockpit's origin can reach the command API,
// so anything that could execute is handed over as a download rather than
// rendered in the page.
func TestOnlyHarmlessTypesRenderInThePage(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"shot.png", "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 40), "inline"},
		{"notes.md", "# what I did", "inline"},
		// A document format that can carry script, which is the one people are
		// surprised by.
		{"chart.svg", "<svg xmlns='http://www.w3.org/2000/svg'></svg>", "attachment"},
		{"report.html", "<h1>hi</h1><script>fetch('/api/projects')</script>", "attachment"},
	}
	for _, tc := range cases {
		h, a, _, _ := served(t, tc.name, tc.body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil))

		got := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: Content-Disposition = %q, want %s", tc.name, got, tc.want)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff; a browser may decide this is something else", tc.name)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: no sandbox policy", tc.name)
		}
	}
}

// An agent picks the filename. A quote or a newline in one is a header
// injection, and the header is written on every download.
func TestAFilenameCannotInjectAHeader(t *testing.T) {
	h, a, _, f := served(t, "ok.png", "\x89PNG\r\n\x1a\nbody")

	// The name an agent chose, written straight into the row the way the
	// socket verb would have.
	hostile := "eq\"; attachment; filename=\"evil\r\nSet-Cookie: x=1"
	if _, err := f.db.SQL().ExecContext(context.Background(),
		`UPDATE artifacts SET name = ? WHERE id = ?`, hostile, a.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil))
	got := rec.Header().Get("Content-Disposition")
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Errorf("Content-Disposition carries a newline: %q", got)
	}
	if strings.Count(got, `"`) != 2 {
		t.Errorf("Content-Disposition has unbalanced quotes: %q", got)
	}
}

// The row can outlive the bytes: retention removes them, or somebody cleans
// the directory. That is neither a fault nor the caller's mistake.
func TestBytesThatAreGoneSaySo(t *testing.T) {
	h, a, blobs, _ := served(t, "report.txt", "gone soon")
	if err := blobs.Remove(a.SHA256); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7717/api/artifacts/"+a.ID+"/bytes", nil))
	if rec.Code != http.StatusGone {
		t.Errorf("status %d, want 410", rec.Code)
	}
}
