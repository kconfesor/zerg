package hatchery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newProject(t *testing.T) (*Hatchery, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	h := New(dir)
	if err := h.EnsureRepo(context.Background(), "main"); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	return h, dir
}

func TestEnsureRepoInitialisesAndCommits(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	// worktree add cannot branch from a repository with no commits, so an
	// initial commit is part of making a directory usable.
	if _, err := git(ctx, dir, "rev-parse", "--verify", "HEAD"); err != nil {
		t.Fatalf("no initial commit was created: %v", err)
	}

	// Idempotent: opening an existing project must not touch its history.
	before, _ := git(ctx, dir, "rev-parse", "HEAD")
	if err := h.EnsureRepo(ctx, "main"); err != nil {
		t.Fatalf("second EnsureRepo: %v", err)
	}
	after, _ := git(ctx, dir, "rev-parse", "HEAD")
	if before != after {
		t.Error("re-opening a project added a commit to it")
	}
}

// Without this, the first role to commit would add every other role's entire
// checkout to the project's history.
func TestWorktreesAreGitIgnored(t *testing.T) {
	_, dir := newProject(t)
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if !strings.Contains(string(body), WorktreesDir+"/") {
		t.Errorf(".gitignore does not exclude worktrees:\n%s", body)
	}
}

func TestEnsureRepoPreservesAnExistingGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("writing .gitignore: %v", err)
	}
	if err := New(dir).EnsureRepo(context.Background(), "main"); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	body, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(body), "node_modules/") {
		t.Error("the project's own ignore rules were discarded")
	}
	if !strings.Contains(string(body), WorktreesDir+"/") {
		t.Error("the worktree rule was not added")
	}
}

func TestEnsureWorktreeCreatesAndReuses(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	path, err := h.EnsureWorktree(ctx, "coder", "main")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if path != filepath.Join(dir, WorktreesDir, "coder") {
		t.Errorf("worktree is at %q", path)
	}

	branch, err := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("reading the worktree branch: %v", err)
	}
	if branch != BranchPrefix+"coder" {
		t.Errorf("worktree is on %q, want %s", branch, BranchPrefix+"coder")
	}

	// Reuse must not discard work in progress: re-creating on every start
	// would throw away whatever an agent was in the middle of.
	scratch := filepath.Join(path, "in-progress.txt")
	if err := os.WriteFile(scratch, []byte("half-done"), 0o644); err != nil {
		t.Fatalf("writing scratch file: %v", err)
	}
	again, err := h.EnsureWorktree(ctx, "coder", "main")
	if err != nil {
		t.Fatalf("second EnsureWorktree: %v", err)
	}
	if again != path {
		t.Errorf("second call returned %q, want the same path", again)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Error("re-opening the worktree destroyed uncommitted work")
	}
}

// Two roles must not be able to collide in one directory.
func TestRolesGetSeparateWorktrees(t *testing.T) {
	h, _ := newProject(t)
	ctx := context.Background()

	coder, err := h.EnsureWorktree(ctx, "coder", "main")
	if err != nil {
		t.Fatalf("coder: %v", err)
	}
	reviewer, err := h.EnsureWorktree(ctx, "reviewer", "main")
	if err != nil {
		t.Fatalf("reviewer: %v", err)
	}
	if coder == reviewer {
		t.Fatal("two roles were given the same worktree")
	}

	if err := os.WriteFile(filepath.Join(coder, "only-mine.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewer, "only-mine.txt")); err == nil {
		t.Error("one role's file appeared in another role's worktree")
	}
}

// A directory that exists but is not a worktree holds someone's data. Removing
// it would be destroying work nobody asked us to touch.
func TestExistingNonWorktreeDirectoryIsRefused(t *testing.T) {
	h, dir := newProject(t)
	path := filepath.Join(dir, WorktreesDir, "coder")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "important.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := h.EnsureWorktree(context.Background(), "coder", "main"); err == nil {
		t.Fatal("an unrelated directory was taken over without a word")
	}
	if _, err := os.Stat(filepath.Join(path, "important.txt")); err != nil {
		t.Error("the refusal still destroyed the directory's contents")
	}
}

// Peer commits must resolve without a fetch — one repository, one object store
// — which is what makes commit-pointer handoffs cheap.
func TestPeerCommitsAreVisibleAcrossWorktrees(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	coder, err := h.EnsureWorktree(ctx, "coder", "main")
	if err != nil {
		t.Fatalf("coder worktree: %v", err)
	}
	if _, err := h.EnsureWorktree(ctx, "reviewer", "main"); err != nil {
		t.Fatalf("reviewer worktree: %v", err)
	}

	if err := os.WriteFile(filepath.Join(coder, "work.txt"), []byte("done"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"add", "."},
		{"commit", "-q", "-m", "work"},
	} {
		if _, err := git(ctx, coder, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	sha, err := h.HeadCommit(ctx, "coder")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	// Readable from the reviewer's worktree with no fetch and no remote.
	if _, err := git(ctx, h.Path("reviewer"), "cat-file", "-t", sha); err != nil {
		t.Errorf("a peer's commit was not resolvable: %v", err)
	}
	_ = dir
}

// Removing a worktree must leave its branch, or commits an agent made would
// become unreachable.
func TestRemoveWorktreeKeepsTheBranch(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	if _, err := h.EnsureWorktree(ctx, "coder", "main"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if err := h.RemoveWorktree(ctx, "coder"); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(h.Path("coder")); err == nil {
		t.Error("the worktree directory is still present")
	}
	if _, err := git(ctx, dir, "rev-parse", "--verify", BranchPrefix+"coder"); err != nil {
		t.Errorf("the role's branch was deleted with its worktree: %v", err)
	}

	// Removing what is not there is not an error.
	if err := h.RemoveWorktree(ctx, "coder"); err != nil {
		t.Errorf("removing an absent worktree failed: %v", err)
	}
}
