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

// A subtask runs on a branch of its own and leaves the role's alone.
//
// The first version of this reset the role's branch --hard to the feature head.
// That isolates the subtask and destroys whatever the role branch was carrying,
// which under `integration = branch` is finished work a person had not landed
// yet and which no other ref points at.
func TestSwitchIsolatesASubtaskWithoutLosingTheRoleBranch(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	kept := commitOnBranch(t, dir, "zerg-coder", "landed-by-hand.txt", "not merged yet\n")
	feature := commitOnBranch(t, dir, "zerg-feature/f1", "feature.txt", "the feature\n")

	tree := filepath.Join(dir, ".worktrees", "coder")
	if _, err := runGit(ctx, dir, "worktree", "add", "-q", "--force", tree, "zerg-coder"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	// Onto the feature, then back again.
	if err := (Git{}).Switch(ctx, tree, "zerg-subtask/coder", feature); err != nil {
		t.Fatalf("Switch onto the feature: %v", err)
	}
	if head, _ := runGit(ctx, tree, "rev-parse", "HEAD"); head != feature {
		t.Errorf("head = %s, want the feature head %s", head, feature)
	}
	if err := (Git{}).Switch(ctx, tree, "zerg-coder", ""); err != nil {
		t.Fatalf("Switch back: %v", err)
	}
	if head, _ := runGit(ctx, tree, "rev-parse", "HEAD"); head != kept {
		t.Errorf("head = %s, want the role branch back at %s", head, kept)
	}
	if _, err := runGit(ctx, dir, "cat-file", "-t", kept); err != nil {
		t.Errorf("the role branch's unlanded commit is gone: %v", err)
	}
}

// The guard that catches what the branch is meant to prevent: base is an
// ancestor of a feature branch, so a commit built on top of the feature
// fast-forwards onto base and takes every commit under it.
func TestContainsSeesAFeatureUnderAnOrdinaryCommit(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	feature := commitOnBranch(t, dir, "zerg-feature/f1", "feature.txt", "the feature\n")

	run := func(args ...string) string {
		t.Helper()
		out, err := runGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	run("checkout", "-q", "-B", "zerg-coder", feature)
	write(t, dir, "ordinary.txt", "an unrelated card\n")
	run("add", ".")
	run("commit", "-q", "-m", "ordinary card")
	ordinary := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")

	carries, err := (Git{}).Contains(ctx, dir, ordinary, feature)
	if err != nil {
		t.Fatalf("Contains: %v", err)
	}
	if !carries {
		t.Error("a commit built on the feature was reported as not carrying it")
	}
	// And --ff-only would have taken it, which is why the check exists.
	if err := (Git{}).Merge(ctx, dir, "main", ordinary); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Skip("git declined the fast-forward, so there is nothing to guard against here")
	}
}

// One conflict must not end a feature: git refuses every later merge in a
// worktree that still has unmerged files.
func TestAbortMergeLetsTheNextIntegrationRun(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	one := commitOnBranch(t, dir, "s1", "f.txt", "one\n")
	two := commitOnBranch(t, dir, "s2", "f.txt", "two\n")

	tree := filepath.Join(dir, ".worktrees", "feature-f1")
	if _, err := runGit(ctx, dir, "worktree", "add", "-q", "-b", "zerg-feature/f1", tree, "main"); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := (Git{}).MergeInto(ctx, tree, one); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if err := (Git{}).MergeInto(ctx, tree, two); err == nil {
		t.Fatal("conflicting merge reported success")
	}
	if err := (Git{}).AbortMerge(ctx, tree); err != nil {
		t.Fatalf("AbortMerge: %v", err)
	}
	// The same merge the wedged tree refused with "Merging is not possible
	// because you have unmerged files".
	if err := (Git{}).MergeInto(ctx, tree, one); err != nil {
		t.Errorf("the feature worktree was left unusable: %v", err)
	}
}
