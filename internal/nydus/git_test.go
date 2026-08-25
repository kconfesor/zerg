package nydus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo builds a real repository with one commit on main and returns its
// path. These tests drive actual git rather than a mock: integration is the
// step that decides whether work reaches the base branch, and a mock of it
// would assert nothing about whether the merge strategy is sound.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		out, err := runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	write(t, dir, "README.md", "start\n")
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// commitOnBranch creates a branch off main, commits to it, and returns the sha
// — the shape a role's worktree produces.
func commitOnBranch(t *testing.T, dir, branch, file, content string) string {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		out, err := runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	run("checkout", "-q", "-b", branch)
	write(t, dir, file, content)
	run("add", ".")
	run("commit", "-q", "-m", "work on "+branch)
	sha := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")
	return sha
}

func TestGitFastForwardsTheBaseBranch(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	sha := commitOnBranch(t, dir, "zerg-reviewer", "feature.txt", "done\n")

	if err := (Git{}).Merge(ctx, dir, "main", sha); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	head, err := runGit(ctx, dir, "rev-parse", "main")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if head != sha {
		t.Errorf("main is at %s, want the integrated commit %s", short(head), short(sha))
	}

	// The merge must not disturb whatever is checked out — agents are working
	// in their own worktrees and integration cannot require one to be idle.
	status, err := runGit(ctx, dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "" {
		t.Errorf("integration left the working tree dirty:\n%s", status)
	}
}

// Completing twice is not a failure: an agent that crashed after merging and
// retried on restart should find the work done.
func TestGitMergeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	sha := commitOnBranch(t, dir, "zerg-reviewer", "feature.txt", "done\n")

	if err := (Git{}).Merge(ctx, dir, "main", sha); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	if err := (Git{}).Merge(ctx, dir, "main", sha); err != nil {
		t.Fatalf("second Merge should be a no-op, got: %v", err)
	}
}

// When the base has moved independently, integration is a human decision.
// Silently picking a side is how work disappears.
func TestGitRefusesWhenTheBaseHasDiverged(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	sha := commitOnBranch(t, dir, "zerg-reviewer", "feature.txt", "done\n")

	// Someone else moves main after the role branched.
	write(t, dir, "other.txt", "meanwhile\n")
	if _, err := runGit(ctx, dir, "add", "."); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := runGit(ctx, dir, "commit", "-q", "-m", "unrelated"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	before, _ := runGit(ctx, dir, "rev-parse", "main")

	err := (Git{}).Merge(ctx, dir, "main", sha)
	if err == nil {
		t.Fatal("a diverged base was integrated without a word")
	}
	if !strings.Contains(err.Error(), "fast-forward") {
		t.Errorf("error should tell the operator what to do, got: %v", err)
	}

	after, _ := runGit(ctx, dir, "rev-parse", "main")
	if after != before {
		t.Error("a refused integration still moved the base branch")
	}
}

func TestGitRejectsAnUnknownCommit(t *testing.T) {
	dir := newRepo(t)
	err := (Git{}).Merge(context.Background(), dir, "main", "0123456789abcdef0123456789abcdef01234567")
	if err == nil {
		t.Fatal("a commit that is not in the repository was accepted")
	}
}

// commitAll commits whatever is in a worktree and returns the sha, which is
// what a role does before handing work on.
func commitAll(t *testing.T, dir, message string) string {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		out, err := runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	run("add", ".")
	run("commit", "-q", "-m", message)
	return strings.TrimSpace(run("rev-parse", "HEAD"))
}
