package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/store"
)

func TestFeatureReviewReadsTheWholeChangeAndKeepsItAfterLanding(t *testing.T) {
	ctx := context.Background()
	h, db, project := newRoutingServer(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	put := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(dir string) string {
		t.Helper()
		git(dir, "add", ".")
		git(dir, "commit", "-m", "work")
		return git(dir, "rev-parse", "HEAD")
	}
	git(project.Path, "init", "-b", "main")
	git(project.Path, "config", "user.email", "test@example.com")
	git(project.Path, "config", "user.name", "Test")
	put(project.Path, "README", "base\n")
	base := commit(project.Path)
	n := nydus.New(db, nydus.WithIntegrator(nydus.Git{}))
	h = New(Deps{DB: db, Nydus: n, Log: slog.Default()}).Routes()
	feature, err := db.CreateFeature(ctx, project.ID, "Coherent delivery", "The original acceptance requirement")
	if err != nil {
		t.Fatal(err)
	}
	hat := hatchery.New(project.Path)
	architect, err := hat.EnsureWorktree(ctx, "supervisor", "main")
	if err != nil {
		t.Fatal(err)
	}
	put(architect, "plan.md", "# Evidence\nThe proposed acceptance checks.\n")
	proof := commit(architect)
	plan, err := n.SubmitFeaturePlan(ctx, store.DecisionScope{ProjectID: project.ID, Role: "supervisor"}, feature.ID, []store.PlanDraft{{Name: "Implementation", Body: "Preserve all requirements", Priority: 70}}, proof)
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, http.MethodPost, "/api/plans/"+plan.ID+"/approve", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	child, err := db.GetTaskByName(ctx, project.ID, "Implementation")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := hat.EnsureWorktree(ctx, "reviewer", "main")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 33; i++ {
		put(tree, fmt.Sprintf("file%02d.txt", i), fmt.Sprintf("requirement %d\n", i))
	}
	commit(tree)
	put(tree, "last.txt", "final check\n")
	head := commit(tree)
	if _, err := n.Send(ctx, project.ID, "reviewer", nydus.SendRequest{TaskID: child.ID, Commit: head, Body: "checked the implementation"}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.ListPendingApprovals(ctx, project.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending integrations: %v %v", pending, err)
	}
	// Even though it is nonterminal, this gate reviews the entire subtask,
	// not just the reviewer's last commit, and merges into the feature's head.
	rec = do(t, h, http.MethodGet, "/api/approvals/"+pending[0].ID+"/diff", nil)
	var diff struct {
		Files []hatchery.ChangedFile
		Base  string
		Range bool
	}
	decodeInto(t, rec, &diff)
	if rec.Code != 200 || !diff.Range || diff.Base != base || len(diff.Files) != 34 {
		t.Fatalf("integration diff: %d %+v", rec.Code, diff)
	}
	if err := n.ApproveBy(ctx, store.DecisionScope{ProjectID: project.ID, Role: "supervisor"}, pending[0].ID, "checked every requirement", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SubmitReview(ctx, feature.ID, head, store.ReviewOK, "checked the original brief and accepted plan", proof); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/features/" + feature.ID
	for _, action := range []string{"land", "refresh", "reject"} {
		rec := do(t, h, http.MethodPost, endpoint+"/"+action, map[string]string{"note": "missing the displayed head"})
		if rec.Code != 400 {
			t.Fatalf("%s without head: %d %s", action, rec.Code, rec.Body)
		}
	}
	rec = do(t, h, http.MethodPost, endpoint+"/reject", map[string]string{"head": head, "note": " "})
	if rec.Code != 400 {
		t.Fatalf("blank correction: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodPost, endpoint+"/land", map[string]string{"head": head})
	if rec.Code != 200 {
		t.Fatalf("land: %d %s", rec.Code, rec.Body)
	}
	if got, err := os.ReadFile(filepath.Join(project.Path, "file00.txt")); err != nil || string(got) != "requirement 0\n" {
		t.Fatalf("base lacks the change: %q %v", got, err)
	}
	rec = do(t, h, http.MethodGet, endpoint, nil)
	var detail store.FeatureDetail
	decodeInto(t, rec, &detail)
	if rec.Code != 200 || detail.Task.State != store.TaskDone || len(detail.Plans) != 1 || detail.Plans[0].ProseSHA != proof || len(detail.Reviews) != 1 {
		t.Fatalf("historical detail: %d %+v", rec.Code, detail)
	}
	rec = do(t, h, http.MethodGet, endpoint+"/files?head="+head, nil)
	decodeInto(t, rec, &diff)
	if rec.Code != 200 || len(diff.Files) != 34 || diff.Base != base {
		t.Fatalf("historical diff was erased by landing: %d %+v", rec.Code, diff)
	}
	var deferred string
	for _, f := range diff.Files {
		if f.Deferred {
			deferred = f.Path
			break
		}
	}
	if deferred == "" {
		t.Fatal("whole change bypassed its eager-file limit")
	}
	rec = do(t, h, http.MethodGet, endpoint+"/files?"+url.Values{"head": {head}, "base": {base}, "path": {deferred}}.Encode(), nil)
	var file hatchery.ChangedFile
	decodeInto(t, rec, &file)
	if rec.Code != 200 || file.Deferred || file.Diff == "" {
		t.Fatalf("deferred file: %d %+v", rec.Code, file)
	}
	rec = do(t, h, http.MethodGet, endpoint+"/files?evidence="+proof, nil)
	decodeInto(t, rec, &diff)
	if rec.Code != 200 || len(diff.Files) != 1 || !strings.Contains(diff.Files[0].Content, "proposed acceptance checks") {
		t.Fatalf("evidence: %d %+v", rec.Code, diff)
	}
	rec = do(t, h, http.MethodGet, endpoint+"/files?head=main", nil)
	if rec.Code != 400 {
		t.Fatal("a mutable branch name was accepted as a reviewed head")
	}
}
