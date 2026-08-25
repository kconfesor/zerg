// Package hatchery manages a project's worktrees.
//
// One linked worktree per role is the predecessor's best idea, carried over
// intact: a single repository and object store, so a peer's commit resolves
// without a fetch, and roles that cannot overwrite each other's files.
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
	if err := h.ensureIgnored(); err != nil {
		return err
	}

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
func (h *Hatchery) ensureIgnored() error {
	path := filepath.Join(h.repoPath, ".gitignore")
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
