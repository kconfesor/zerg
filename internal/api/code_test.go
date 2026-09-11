package api

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/hatchery"
)

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCodeExplorerBrowsesRefsTreeAndFile(t *testing.T) {
	h, _, project := newRoutingServer(t)
	dir := project.Path

	gitAt(t, dir, "init", "-b", "main")
	gitAt(t, dir, "config", "user.email", "test@example.com")
	gitAt(t, dir, "config", "user.name", "Test")
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "readme.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitAt(t, dir, "add", ".")
	gitAt(t, dir, "commit", "-m", "add docs")
	gitAt(t, dir, "tag", "v1")
	head := gitAt(t, dir, "rev-parse", "HEAD")

	// refs: main and v1 both show up.
	rec := do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/refs", nil)
	var refs []hatchery.Ref
	decodeInto(t, rec, &refs)
	if rec.Code != 200 {
		t.Fatalf("refs: %d %s", rec.Code, rec.Body)
	}
	by := map[string]hatchery.Ref{}
	for _, r := range refs {
		by[r.Name] = r
	}
	if by["main"].Kind != "branch" || by["v1"].Kind != "tag" {
		t.Fatalf("refs = %+v, want main (branch) and v1 (tag)", refs)
	}

	// tree: the root lists docs as a directory.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/tree?"+url.Values{"ref": {"main"}}.Encode(), nil)
	var tree struct {
		ResolvedSha string
		Entries     []hatchery.TreeEntry
	}
	decodeInto(t, rec, &tree)
	if rec.Code != 200 || tree.ResolvedSha != head {
		t.Fatalf("tree at root: %d %+v, want resolvedSha %s", rec.Code, tree, head)
	}
	if len(tree.Entries) != 1 || tree.Entries[0].Name != "docs" || tree.Entries[0].Kind != "tree" {
		t.Fatalf("root entries = %+v, want one tree named docs", tree.Entries)
	}

	// tree: a subdirectory lists its file, not just itself.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/tree?"+url.Values{"ref": {"main"}, "path": {"docs"}}.Encode(), nil)
	decodeInto(t, rec, &tree)
	if rec.Code != 200 || len(tree.Entries) != 1 || tree.Entries[0].Path != "docs/readme.txt" {
		t.Fatalf("docs entries = %d %+v, want one file docs/readme.txt", rec.Code, tree.Entries)
	}

	// file: reads the blob at the pinned sha.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/file?"+url.Values{"ref": {"main"}, "path": {"docs/readme.txt"}}.Encode(), nil)
	var file struct {
		ResolvedSha string
		Blob        hatchery.Blob
	}
	decodeInto(t, rec, &file)
	if rec.Code != 200 || file.ResolvedSha != head || file.Blob.Content != "hello\n" {
		t.Fatalf("file: %d %+v", rec.Code, file)
	}

	// A ref this repository does not have is the operator's problem, not a 500.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/tree?"+url.Values{"ref": {"no-such-branch"}}.Encode(), nil)
	if rec.Code != 400 {
		t.Fatalf("unknown ref: %d %s, want 400", rec.Code, rec.Body)
	}

	// A path this repository does not have at a real ref is also a 400.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/file?"+url.Values{"ref": {"main"}, "path": {"nope.txt"}}.Encode(), nil)
	if rec.Code != 400 {
		t.Fatalf("unknown path: %d %s, want 400", rec.Code, rec.Body)
	}

	// A path-shaped ref that resolve() would have echoed back is rejected, not
	// silently browsed as if it were a real revision.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/tree?"+url.Values{"ref": {"docs"}}.Encode(), nil)
	if rec.Code != 400 {
		t.Fatalf("path-shaped ref: %d %s, want 400", rec.Code, rec.Body)
	}

	// ref and path are both required.
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/tree", nil)
	if rec.Code != 400 {
		t.Fatalf("missing ref: %d %s, want 400", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/file?ref=main", nil)
	if rec.Code != 400 {
		t.Fatalf("missing path: %d %s, want 400", rec.Code, rec.Body)
	}
}

// newRoutingServer builds no chat.Manager, so explain has no agent to ask --
// the same shape askAboutTheChange/requestGuide are in when a build has none.
func TestRepoExplainWithNoAgentSaysSoRatherThanFailing(t *testing.T) {
	h, _, project := newRoutingServer(t)

	rec := do(t, h, http.MethodPost, "/api/projects/"+project.ID+"/explain",
		map[string]string{"ref": "main", "path": "README.md"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("explain with no chat manager: %d %s, want 501", rec.Code, rec.Body)
	}

	rec = do(t, h, http.MethodGet, "/api/projects/"+project.ID+"/explain", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("polling a mistyped path: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestExplainJobsAnswerOnlyTheProjectThatStartedThem(t *testing.T) {
	j := newExplainJobs()
	id := j.start("project-a")

	if _, ok := j.get("project-b", id); ok {
		t.Error("a job answered for a project that did not start it")
	}
	if view, ok := j.get("project-a", id); !ok || view.Status != string(explainReading) {
		t.Errorf("get = %+v, %v, want status %q", view, ok, explainReading)
	}

	j.finish(id, "it mounts the app")
	view, ok := j.get("project-a", id)
	if !ok || view.Status != string(explainDone) || view.Answer != "it mounts the app" {
		t.Errorf("after finish: %+v, %v", view, ok)
	}

	other := j.start("project-a")
	j.fail(other, "the agent is in the middle of an answer")
	view, ok = j.get("project-a", other)
	if !ok || view.Status != string(explainFailed) || view.Error == "" {
		t.Errorf("after fail: %+v, %v", view, ok)
	}

	if _, ok := j.get("project-a", "no-such-job"); ok {
		t.Error("an unknown job id was answered")
	}
}

func TestExplainJobsForgetAnswersPastTheirTTL(t *testing.T) {
	j := newExplainJobs()
	id := j.start("project-a")
	j.finish(id, "answer")
	// Backdate it past the TTL rather than waiting for one, or mocking time.
	j.mu.Lock()
	j.byID[id].at = time.Now().Add(-explainJobTTL - time.Minute)
	j.mu.Unlock()

	// The sweep runs on start, not on get -- starting a new job is what a
	// real client causes by asking another question, which is when it is
	// safe to say the old one is gone.
	j.start("project-a")
	if _, ok := j.get("project-a", id); ok {
		t.Error("an expired job was still answered")
	}
}

func TestExplainPromptsNameWhatIsBeingExplained(t *testing.T) {
	whole := explainPrompt("main", "src/data/paintings.ts")
	if !strings.Contains(whole, "main") || !strings.Contains(whole, "src/data/paintings.ts") {
		t.Errorf("explainPrompt did not name the ref and path: %q", whole)
	}

	selection := explainSelectionPrompt("main", "src/data/paintings.ts", "export const paintings = []", "")
	if !strings.Contains(selection, "export const paintings = []") {
		t.Errorf("explainSelectionPrompt did not embed the selection: %q", selection)
	}
	if !strings.Contains(selection, "Explain what this does.") {
		t.Errorf("an empty question should fall back to a default, got: %q", selection)
	}

	asked := explainSelectionPrompt("main", "x.ts", "const x = 1", "why is this exported?")
	if !strings.Contains(asked, "why is this exported?") {
		t.Errorf("explainSelectionPrompt dropped the actual question: %q", asked)
	}
}
