package hatchery

import (
	"context"
	"errors"
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
// Worktrees must be excluded, and the exclusion must not sit in the project's
// own .gitignore — see TestWorktreeRuleDoesNotBlockAnIncomingGitignore for what
// that cost. The check that matters is git's answer, not the file's contents.
func TestWorktreesAreGitIgnored(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()
	if _, err := h.EnsureWorktree(ctx, "coder", "main"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, err := git(ctx, dir, "check-ignore", WorktreesDir+"/"); err != nil {
		t.Errorf("git does not ignore %s/: %v", WorktreesDir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Error("zerg wrote a .gitignore into the project")
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

	// The project's .gitignore is theirs and must come through untouched.
	body, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(body), "node_modules/") {
		t.Error("the project's own ignore rules were discarded")
	}
	if strings.Contains(string(body), WorktreesDir+"/") {
		t.Error("zerg's rule was written into the project's .gitignore")
	}

	// It belongs in info/exclude, which is per-repository and never committed.
	excl, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("reading info/exclude: %v", err)
	}
	if !strings.Contains(string(excl), WorktreesDir+"/") {
		t.Error("the worktree rule was not added to info/exclude")
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

// The rule used to be written to an uncommitted .gitignore, which collides with
// the .gitignore that cargo, npm, pip and go all generate: the first hand-off
// carries a commit adding one, and merge refuses to overwrite an untracked file
// of the same name. Deterministic on every real project, and invisible to the
// old test, which only checked the rule was written somewhere.
func TestWorktreeRuleDoesNotBlockAnIncomingGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()

	// The repository already has history, which is the case that matters and
	// the one the first version of this test missed. On an empty directory
	// EnsureRepo lays down an initial commit and sweeps its own .gitignore
	// into it, leaving the file tracked and harmless; a project that already
	// has a commit gets the file written and left untracked, and that is what
	// blocks the merge. Real projects are adopted, not created.
	if _, err := git(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "Initial commit"},
	} {
		if _, err := git(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	h := New(dir)
	if err := h.EnsureRepo(ctx, "main"); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if _, err := h.EnsureWorktree(ctx, "coder", "main"); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// The coder does what `cargo init` does: adds a .gitignore and commits it.
	tree := h.Path("coder")
	if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("/target\n"), 0o644); err != nil {
		t.Fatalf("writing the role's .gitignore: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "cargo init"}} {
		if _, err := git(ctx, tree, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	sha, err := h.HeadCommit(ctx, "coder")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}

	// Integration into the base branch must not be blocked by zerg's own file.
	if _, err := git(ctx, dir, "merge", "--ff-only", strings.TrimSpace(sha)); err != nil {
		t.Fatalf("integration blocked by zerg's own ignore file: %v", err)
	}
}

// A project created before the rule moved still carries the stray file and
// would be blocked by it forever. It is removed only when untracked and saying
// nothing else; anything more is the project's own file.
func TestLegacyIgnoreIsClearedOnlyWhenItIsOurs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	h := New(dir)
	if err := h.EnsureRepo(ctx, "main"); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	// Untracked, but carrying the project's rules too: not ours to remove.
	mixed := WorktreesDir + "/\nnode_modules/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureRepo(ctx, "main"); err != nil {
		t.Fatalf("EnsureRepo (second): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err != nil {
		t.Error("removed a .gitignore that carried the project's own rules")
	}

	// Untracked and saying only our entry: ours, and it must go.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(WorktreesDir+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureRepo(ctx, "main"); err != nil {
		t.Fatalf("EnsureRepo (third): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Error("the stray file we wrote was left in place")
	}
}

// The sweep must take build output and leave everything else. A role's
// worktree is mostly regenerable bytes — 256 KB of source against 45 MB of
// target/ in a real Rust project — but an agent's uncommitted work lives in
// untracked files, and those are not ours to delete.
func TestSweepTakesIgnoredFilesAndNothingElse(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	h, _ := newProject(t)
	tree, err := h.EnsureWorktree(ctx, "coder", "main")
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// The project declares its build directory disposable, and commits that.
	if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("target/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "ignore target"}} {
		if _, err := git(ctx, tree, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	if err := os.MkdirAll(filepath.Join(tree, "target", "debug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "target", "debug", "big"), make([]byte, 64*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked but not ignored: an agent's work in progress.
	if err := os.WriteFile(filepath.Join(tree, "draft.rs"), []byte("fn wip() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	freed, err := h.SweepIgnored(ctx, "coder")
	if err != nil {
		t.Fatalf("SweepIgnored: %v", err)
	}
	if freed < 64*1024 {
		t.Errorf("freed %d bytes, want at least the 64 KB of build output", freed)
	}
	if _, err := os.Stat(filepath.Join(tree, "target")); !os.IsNotExist(err) {
		t.Error("ignored build output survived the sweep")
	}
	if _, err := os.Stat(filepath.Join(tree, "draft.rs")); err != nil {
		t.Error("the sweep deleted untracked work; only ignored files are ours to remove")
	}
	if _, err := os.Stat(filepath.Join(tree, ".gitignore")); err != nil {
		t.Error("the sweep deleted a tracked file")
	}
}

// Only branches whose work is already on the base branch, and only ours.
func TestPruneTakesMergedZergBranchesOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	h, dir := newProject(t)

	// A merged role branch, made without a worktree so nothing holds it.
	if _, err := git(ctx, dir, "branch", BranchPrefix+"merged", "main"); err != nil {
		t.Fatal(err)
	}
	// A role branch carrying work that never reached main.
	if _, err := git(ctx, dir, "checkout", "-q", "-b", BranchPrefix+"ahead"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("unmerged"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "ahead"}} {
		if _, err := git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(ctx, dir, "checkout", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	// And one of the operator's own, merged, which is not ours to touch.
	if _, err := git(ctx, dir, "branch", "my-feature", "main"); err != nil {
		t.Fatal(err)
	}

	pruned, err := h.PruneMergedBranches(ctx, "main")
	if err != nil {
		t.Fatalf("PruneMergedBranches: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != BranchPrefix+"merged" {
		t.Fatalf("pruned %v, want only %s", pruned, BranchPrefix+"merged")
	}

	out, _ := git(ctx, dir, "branch", "--format=%(refname:short)")
	for _, must := range []string{"my-feature", BranchPrefix + "ahead"} {
		if !strings.Contains(out, must) {
			t.Errorf("%s was deleted; it is either unmerged or not zerg's", must)
		}
	}
}

// Tests here make their own commits, and a machine with no configured identity
// — a fresh container, a CI runner — makes git refuse them with "Please tell me
// who you are". The product sets an identity for the one commit it authors
// itself (see EnsureRepo); this is the same problem on the test's side of the
// line, so it is fixed for the whole binary rather than at each call.
//
// Environment rather than config: it touches no repository and no file, and it
// cannot leak into the operator's own commits.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":     "zerg tests",
		"GIT_AUTHOR_EMAIL":    "tests@localhost",
		"GIT_COMMITTER_NAME":  "zerg tests",
		"GIT_COMMITTER_EMAIL": "tests@localhost",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

// A ref this repository does not have is reported as that, and separately from
// everything else that can go wrong, because the two are answered differently:
// one is the operator's to fix, the other is the daemon's.
func TestChangedFilesSeparatesUnknownRevisionsFromRealFailures(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a commit that does not exist", func(t *testing.T) {
		_, err := h.ChangedFiles(ctx, "0000000000000000000000000000000000000000", 0)
		if !errors.Is(err, ErrNoSuchRevision) {
			t.Errorf("err = %v, want ErrNoSuchRevision", err)
		}
	})

	t.Run("a base branch that does not exist", func(t *testing.T) {
		_, err := h.RangeFiles(ctx, "no-such-branch", head, 0)
		if !errors.Is(err, ErrNoSuchRevision) {
			t.Errorf("err = %v, want ErrNoSuchRevision", err)
		}
		if !strings.Contains(err.Error(), "no-such-branch") {
			t.Errorf("err = %v, want it to name the branch", err)
		}
	})

	t.Run("a repository that is not one", func(t *testing.T) {
		// Operational: git runs and fails for a reason that has nothing to do
		// with the revision, which must not be reported as a missing revision.
		other := New(t.TempDir())
		_, err := other.ChangedFiles(ctx, head, 0)
		if err == nil {
			t.Fatal("reading a diff out of a non-repository succeeded")
		}
		if errors.Is(err, ErrNoSuchRevision) {
			t.Errorf("err = %v; a broken repository is not a missing revision", err)
		}
	})

	t.Run("a revision that does exist", func(t *testing.T) {
		if _, err := h.ChangedFiles(ctx, head, 0); err != nil {
			t.Errorf("ChangedFiles on HEAD: %v", err)
		}
	})
}

// Whether the work still merges, asked without touching a worktree.
//
// An approval decides whether work lands, and nothing said whether it could.
// `git merge-tree --write-tree` does the merge in memory, so the answer costs
// nothing and leaves nothing behind: no checkout, no index, nothing to clean up
// when it conflicts.
func TestMergeCheckAnswersBeforeAnythingIsMerged(t *testing.T) {
	h, dir := newProject(t)
	ctx := context.Background()

	write := func(t *testing.T, name, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := git(ctx, dir, "add", "."); err != nil {
			t.Fatal(err)
		}
		if _, err := git(ctx, dir, "commit", "-m", message); err != nil {
			t.Fatal(err)
		}
		sha, err := git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}

	write(t, "f.txt", "one\ntwo\nthree\n", "base")
	if _, err := git(ctx, dir, "checkout", "-b", "work"); err != nil {
		t.Fatal(err)
	}
	work := write(t, "f.txt", "ONE\ntwo\nthree\n", "the work")
	if _, err := git(ctx, dir, "checkout", "main"); err != nil {
		t.Fatal(err)
	}

	// Nothing has moved, so it lands.
	clean, err := h.MergeCheck(ctx, "main", work)
	if err != nil {
		t.Fatalf("MergeCheck: %v", err)
	}
	if !clean.Clean || len(clean.Conflicts) != 0 || clean.BaseAhead != 0 {
		t.Errorf("a merge into an unchanged base came back %+v", clean)
	}

	// The base takes a commit on the same lines, which is the ordinary way an
	// approval that looked clean fails at the merge.
	write(t, "f.txt", "uno\ntwo\nthree\n", "the base moves")
	conflicted, err := h.MergeCheck(ctx, "main", work)
	if err != nil {
		t.Fatalf("MergeCheck after the base moved: %v", err)
	}
	if conflicted.Clean {
		t.Error("a conflicting merge came back clean")
	}
	if len(conflicted.Conflicts) != 1 || conflicted.Conflicts[0] != "f.txt" {
		t.Errorf("conflicts are %v, want the one file: git reports it once per stage", conflicted.Conflicts)
	}
	// Worth saying on its own: reviewing a diff taken against a base that has
	// since moved is reviewing something other than what will land.
	if conflicted.BaseAhead != 1 {
		t.Errorf("base is %d commits ahead, want 1", conflicted.BaseAhead)
	}

	// And the check leaves nothing behind: no merge in progress, no changes.
	if status, err := git(ctx, dir, "status", "--porcelain"); err != nil || status != "" {
		t.Errorf("the check left the repository dirty: %q (%v)", status, err)
	}

	// A ref this repository does not have is the operator's problem, and is
	// answerable as one rather than as a fault.
	if _, err := h.MergeCheck(ctx, "main", "0000000000000000000000000000000000000000"); !errors.Is(err, ErrNoSuchRevision) {
		t.Errorf("an unknown commit gave %v, want ErrNoSuchRevision", err)
	}
}
