// Package hatchery manages a project's worktrees.
//
// One linked worktree per role: a single repository and object store, so a
// peer's commit resolves without a fetch, and roles that cannot overwrite each
// other's files.
package hatchery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreesDir is where role worktrees live inside a project.
const WorktreesDir = ".worktrees"

// BranchPrefix keeps role branches recognisable and out of the way of whatever
// naming a project already uses.
const BranchPrefix = "zerg-"

// Hatchery prepares worktrees for one project.
type Hatchery struct {
	repoPath string
}

func New(repoPath string) *Hatchery { return &Hatchery{repoPath: repoPath} }

// Path is where a role works. The role name is constrained where it enters the
// system — lowercase, dashes, no separators — so it is safe to join here
// without re-sanitising.
func (h *Hatchery) Path(role string) string {
	return filepath.Join(h.repoPath, WorktreesDir, role)
}

// EnsureRepo initialises the project as a git repository if it is not one.
//
// A brand-new repository has no commits, and `git worktree add` cannot branch
// from nothing, so this also lays down an initial commit.
func (h *Hatchery) EnsureRepo(ctx context.Context, baseBranch string) error {
	if _, err := git(ctx, h.repoPath, "rev-parse", "--git-dir"); err != nil {
		if _, err := git(ctx, h.repoPath, "init", "-q", "-b", baseBranch); err != nil {
			return fmt.Errorf("initialising a repository in %s: %w", h.repoPath, err)
		}
	}
	if err := h.ensureIgnored(ctx); err != nil {
		return err
	}
	h.clearLegacyIgnore(ctx)

	if _, err := git(ctx, h.repoPath, "rev-parse", "--verify", "HEAD"); err == nil {
		return nil // already has history
	}
	if _, err := git(ctx, h.repoPath, "add", "-A"); err != nil {
		return fmt.Errorf("staging the initial commit: %w", err)
	}
	if _, err := git(ctx, h.repoPath, "commit", "-q", "--allow-empty", "-m", "Initial commit"); err != nil {
		return fmt.Errorf("creating the initial commit: %w", err)
	}
	return nil
}

// ensureIgnored keeps worktrees out of the project's own history. Without this
// the first role to commit would add every other role's checkout to the repo.
//
// The rule goes in .git/info/exclude, not .gitignore, because .worktrees/ is
// zerg's business and not a fact about the project. .gitignore is the
// project's file: it is tracked, it is reviewed, and it belongs in the
// project's history.
//
// This was originally written to .gitignore, uncommitted, which broke every
// first task on a real project. Cargo, npm, pip and go all produce a
// .gitignore, so the first hand-off carried a commit adding one, and
// `merge --ff-only` refuses to overwrite an untracked file of the same name:
//
//	error: The following untracked working tree files would be overwritten
//	by merge: .gitignore
//
// Deterministic, not a race, and unfixable by the agent that hit it — the
// reviewer that found it correctly refused to touch the operator's checkout
// and escalated instead. info/exclude is per-repository, never committed,
// applies to every linked worktree, and cannot collide with a tracked file.
func (h *Hatchery) ensureIgnored(ctx context.Context) error {
	// --git-common-dir, not --git-dir: inside a linked worktree the latter is
	// that worktree's private directory, whose info/exclude the other
	// worktrees would not read.
	common, err := git(ctx, h.repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("locating the git directory: %w", err)
	}
	gitDir := strings.TrimSpace(common)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(h.repoPath, gitDir)
	}

	dir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "exclude")

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	entry := WorktreesDir + "/"
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += entry + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// clearLegacyIgnore removes the .gitignore earlier versions wrote, so a project
// created before the rule moved is not blocked forever by a file it never
// asked for.
//
// Only when it is untracked and says nothing but the worktrees entry: anything
// else is the project's own file, and zerg does not edit those.
func (h *Hatchery) clearLegacyIgnore(ctx context.Context) {
	path := filepath.Join(h.repoPath, ".gitignore")
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) != 1 || lines[0] != WorktreesDir+"/" {
		return
	}
	// Tracked means the project adopted it; it is theirs now.
	if _, err := git(ctx, h.repoPath, "ls-files", "--error-unmatch", ".gitignore"); err == nil {
		return
	}
	os.Remove(path)
}

// EnsureWorktree creates (or reuses) a role's worktree and returns its path.
//
// Idempotent: an existing worktree at the right path is left exactly as it is,
// uncommitted work included. Re-creating it on every start would throw away
// whatever an agent was in the middle of.
func (h *Hatchery) EnsureWorktree(ctx context.Context, role, baseBranch string) (string, error) {
	path := h.Path(role)
	branch := BranchPrefix + role

	if isWorktree(ctx, path) {
		return path, nil
	}
	// A directory that exists but is not a worktree is someone else's data.
	// Removing it would be destroying work nobody asked us to touch.
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists and is not a git worktree; move it aside", path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// -B resets the role branch to the base on each fresh creation, so a role
	// starts from current work rather than from wherever it left off weeks ago.
	if _, err := git(ctx, h.repoPath, "worktree", "add", "--force", "-B", branch, path, baseBranch); err != nil {
		return "", fmt.Errorf("creating a worktree for %s: %w", role, err)
	}
	return path, nil
}

// RemoveWorktree detaches a role's worktree, leaving its branch behind so any
// commits it made remain reachable.
func (h *Hatchery) RemoveWorktree(ctx context.Context, role string) error {
	path := h.Path(role)
	if !isWorktree(ctx, path) {
		return nil
	}
	if _, err := git(ctx, h.repoPath, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("removing the worktree for %s: %w", role, err)
	}
	return nil
}

// HeadCommit returns the full sha at a worktree's HEAD.
func (h *Hatchery) HeadCommit(ctx context.Context, role string) (string, error) {
	return git(ctx, h.Path(role), "rev-parse", "HEAD")
}

func isWorktree(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	_, err := git(ctx, path, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// SweepIgnored removes files the project itself declared disposable from a
// role's worktree, and reports how many bytes went.
//
// `git clean -Xdf` and never -x: capital X removes only ignored files, so it
// deletes build output and nothing a project has not already said is
// regenerable. Lowercase -x would also take untracked files, which is where an
// agent's half-finished work lives.
//
// The numbers are the argument. A Rust calculator's worktree is 256 KB of
// source and 45 MB of target/ — per role, so it multiplies by team size, and
// none of it is worth keeping once a task is done.
func (h *Hatchery) SweepIgnored(ctx context.Context, role string) (int64, error) {
	path := h.Path(role)
	if !isWorktree(ctx, path) {
		return 0, nil
	}
	before := dirSize(path)
	if _, err := git(ctx, path, "clean", "-Xdf"); err != nil {
		return 0, fmt.Errorf("sweeping %s: %w", path, err)
	}
	freed := before - dirSize(path)
	if freed < 0 {
		freed = 0
	}
	return freed, nil
}

// PruneMergedBranches deletes zerg-<role> branches already contained in the
// base branch, and returns the names.
//
// Only merged ones: -d rather than -D, so git refuses anything carrying work
// that has not reached the base branch. A branch whose worktree is still
// checked out is skipped by git for the same reason.
func (h *Hatchery) PruneMergedBranches(ctx context.Context, baseBranch string) ([]string, error) {
	out, err := git(ctx, h.repoPath, "branch", "--merged", baseBranch, "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("listing merged branches: %w", err)
	}

	var pruned []string
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		// Ours only. Deleting a branch the operator made would be well past
		// anything this was asked to do.
		if name == "" || !strings.HasPrefix(name, BranchPrefix) || name == baseBranch {
			continue
		}
		if _, err := git(ctx, h.repoPath, "branch", "-d", name); err != nil {
			continue // checked out, or not actually merged; either way leave it
		}
		pruned = append(pruned, name)
	}
	return pruned, nil
}

// dirSize is best effort: a tree that cannot be walked reports what it read, so
// a permissions problem understates the saving rather than failing the sweep.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Subject is the first line of a commit's message.
//
// Best effort: a sha that is not in the repository returns empty rather than an
// error, because a missing subject should degrade a detail view, not fail it.
func (h *Hatchery) Subject(ctx context.Context, sha string) string {
	out, err := git(ctx, h.repoPath, "log", "-1", "--format=%s", sha)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Diff returns what a commit changed, as unified diff text.
//
// Capped, because an approval needs to show the work and a browser does not
// need a megabyte of it: past the cap the reader is better served by the file
// than by more diff. Best effort — a sha that is not here returns empty rather
// than an error, so a missing diff degrades the view instead of failing it.
func (h *Hatchery) Diff(ctx context.Context, sha string, maxBytes int) (string, bool) {
	out, err := git(ctx, h.repoPath, "show", "--format=", "--patch", sha)
	if err != nil {
		return "", false
	}
	if maxBytes > 0 && len(out) > maxBytes {
		return out[:maxBytes], true
	}
	return out, false
}

// ChangedFile is one file a commit touched.
type ChangedFile struct {
	Path string `json:"path"`
	// Status is git's letter: A added, M modified, D deleted.
	Status string `json:"status"`
	// Content is the file as of this commit. Empty for a deletion, and for
	// anything too large or not text — the diff is the fallback for those.
	Content string `json:"content,omitempty"`
	// Diff is the unified diff for this file, which is what you want for a
	// change to existing code and not what you want for a new document.
	Diff string `json:"diff,omitempty"`
}

// ChangedFiles returns what a commit touched, with the content of each file as
// of that commit.
//
// Content as well as diff, because the two answer different questions. A diff
// is right for a change to existing code. For a file the commit created — a
// spec, a design note — the diff is the document with a plus in front of every
// line, and the thing you actually want to read is the document.
func (h *Hatchery) ChangedFiles(ctx context.Context, sha string, maxBytes int) ([]ChangedFile, error) {
	return h.changed(ctx, sha, "", maxBytes)
}

// RangeFiles returns everything between base and sha — what would land if the
// commit were merged.
//
// This is the question at the final gate, and it is a different question from
// "what did the last role write". A task takes several commits across several
// roles, and approving the last one while seeing only its diff is approving a
// merge on the strength of its final paragraph.
//
// base...sha, three dots: the changes on sha's side since the two diverged,
// not everything that happened on base meanwhile. Two dots would show the base
// branch's own progress as if this task had made it.
func (h *Hatchery) RangeFiles(ctx context.Context, base, sha string, maxBytes int) ([]ChangedFile, error) {
	return h.changed(ctx, sha, base, maxBytes)
}

func (h *Hatchery) changed(ctx context.Context, sha, base string, maxBytes int) ([]ChangedFile, error) {
	var out string
	var err error
	if base != "" {
		out, err = git(ctx, h.repoPath, "diff", "--name-status", base+"..."+sha)
	} else {
		out, err = git(ctx, h.repoPath, "show", "--format=", "--name-status", sha)
	}
	if err != nil {
		return nil, fmt.Errorf("listing what %s changed: %w", sha, err)
	}

	var files []ChangedFile
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		f := ChangedFile{Status: parts[0][:1], Path: parts[len(parts)-1]}

		if f.Status != "D" {
			if body, err := git(ctx, h.repoPath, "show", sha+":"+f.Path); err == nil {
				if maxBytes <= 0 || len(body) <= maxBytes {
					f.Content = body
				}
			}
		}
		// The diff comes too: content alone cannot show what changed in a file
		// that already existed.
		var d string
		var derr error
		if base != "" {
			d, derr = git(ctx, h.repoPath, "diff", base+"..."+sha, "--", f.Path)
		} else {
			d, derr = git(ctx, h.repoPath, "show", "--format=", "--patch", sha, "--", f.Path)
		}
		if derr == nil && (maxBytes <= 0 || len(d) <= maxBytes) {
			f.Diff = d
		}
		files = append(files, f)
	}
	return files, nil
}
