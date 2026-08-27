package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrowseListsDirsAndFlagsRepos asserts the effect: given a directory with a
// git repo, a plain folder and a file, the browse endpoint returns the two
// directories with the repo flagged, excludes the file, and reports the parent
// to walk up to.
func TestBrowseListsDirsAndFlagsRepos(t *testing.T) {
	h, _ := newTestServer(t)

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "myrepo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree's .git is a file, not a directory; isRepo must catch both, so
	// mark the repo the harder way.
	if err := os.WriteFile(filepath.Join(root, "myrepo", ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape(root), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Path    string        `json:"path"`
		Parent  string        `json:"parent"`
		Entries []browseEntry `json:"entries"`
	}
	decodeInto(t, rec, &got)

	if got.Path != root {
		t.Errorf("path = %q, want %q", got.Path, root)
	}
	if got.Parent != filepath.Dir(root) {
		t.Errorf("parent = %q, want %q", got.Parent, filepath.Dir(root))
	}

	byName := map[string]browseEntry{}
	for _, e := range got.Entries {
		byName[e.Name] = e
	}
	if len(byName) != 2 {
		t.Fatalf("entries = %v, want exactly myrepo and plain (file and dotfile excluded)", got.Entries)
	}
	if _, ok := byName["notes.txt"]; ok {
		t.Error("a file was listed; only directories should be")
	}
	if _, ok := byName[".hidden"]; ok {
		t.Error("a dotted directory was listed; it should be skipped")
	}
	if e, ok := byName["myrepo"]; !ok || !e.IsRepo {
		t.Errorf("myrepo = %+v, want listed with IsRepo=true", e)
	}
	if e := byName["plain"]; e.IsRepo {
		t.Errorf("plain flagged as a repo, want IsRepo=false")
	}
	if e := byName["myrepo"]; e.Path != filepath.Join(root, "myrepo") {
		t.Errorf("myrepo path = %q, want absolute join", e.Path)
	}
}

// TestBrowseRejectsRelativePath: a relative path has no meaning to the daemon,
// so it is an operator error the endpoint names, not a 500.
func TestBrowseRejectsRelativePath(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape("some/relative/dir"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestBrowseMissingDirIs400: a folder that is not there is fixable by the
// operator, so it comes back as a named 400 rather than a fault.
func TestBrowseMissingDirIs400(t *testing.T) {
	h, _ := newTestServer(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape(missing), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// A symlinked directory is a directory. ReadDir describes the link rather than
// what it points at, so the obvious check leaves `~/src -> /Volumes/Work/src`
// out of the picker with nothing on screen to say why.
func TestBrowseFollowsSymlinkedDirectories(t *testing.T) {
	h, _ := newTestServer(t)
	root := t.TempDir()

	real := filepath.Join(root, "elsewhere", "repo")
	if err := os.MkdirAll(filepath.Join(real, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "linked-repo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A link to a file is not a directory, and a broken one is not either.
	file := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(root, "linked-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape(root), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Entries []struct {
			Name   string `json:"name"`
			IsRepo bool   `json:"isRepo"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, e := range got.Entries {
		found[e.Name] = e.IsRepo
	}
	if isRepo, ok := found["linked-repo"]; !ok {
		t.Errorf("entries = %v; a symlinked repository must be listed", found)
	} else if !isRepo {
		t.Error("linked-repo was listed but not flagged as a repository")
	}
	for _, name := range []string{"linked-file", "broken", "notes.txt"} {
		if _, ok := found[name]; ok {
			t.Errorf("%s was listed; only directories belong in a folder picker", name)
		}
	}
}

// A directory with more folders than anyone will scroll is cut short, and says
// so. Hiding the cut would hide the folder being looked for.
func TestBrowseBoundsALargeDirectory(t *testing.T) {
	h, _ := newTestServer(t)
	root := t.TempDir()
	for i := 0; i < maxEntries+50; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%05d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape(root), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Entries   []struct{ Name string } `json:"entries"`
		Truncated bool                    `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != maxEntries {
		t.Errorf("returned %d entries, want the cap of %d", len(got.Entries), maxEntries)
	}
	if !got.Truncated {
		t.Error("the listing was cut short and did not say so")
	}
}

// `~bob` is another user's home, which this does not resolve. Treating it as a
// prefix produced a path nobody typed and reported that back as missing.
func TestBrowseDoesNotManglePathsBeginningWithTilde(t *testing.T) {
	h, _ := newTestServer(t)
	rec := do(t, h, "GET", "/api/browse?path="+url.QueryEscape("~bob/src"), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "~bob/src") {
		t.Errorf("error = %s; it should name what was asked for", body)
	}
}
