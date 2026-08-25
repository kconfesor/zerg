package nydus

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Git integrates a terminal commit into the project's base branch by shelling
// out to git.
//
// Integration is the orchestrator's job, not an agent's. Having the last role
// broadcast its commit to every other role to merge means N chances to conflict
// and no single place that knows whether the base branch actually moved.
type Git struct{}

// Merge integrates commit into baseBranch inside repoPath.
//
// repoPath is the project root, which holds the base branch; roles work in
// linked worktrees under .worktrees/, so integrating here disturbs nobody.
//
// It uses `merge --ff-only` rather than moving the ref directly. Moving a
// checked-out branch with update-ref advances HEAD while leaving the index and
// working tree describing the old commit, so the repository is left reporting
// phantom deletions for every file the new commit added. merge updates ref,
// index and working tree together, and refuses outright if the tree is dirty.
func (Git) Merge(ctx context.Context, repoPath, baseBranch, commit string) error {
	if err := verifyCommit(ctx, repoPath, commit); err != nil {
		return err
	}

	// Already contained: completing twice is not a failure. An agent that
	// crashed after merging and retried should find the work done, not an error.
	if _, err := runGit(ctx, repoPath, "merge-base", "--is-ancestor", commit, baseBranch); err == nil {
		return nil
	}

	// The merge lands on whatever this checkout has, so confirm it is the
	// branch we were asked to integrate into rather than silently merging
	// somewhere else.
	current, err := runGit(ctx, repoPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the checked-out branch: %w", err)
	}
	if current != baseBranch {
		return fmt.Errorf("%s has %s checked out, not the base branch %s; integration would land on the wrong branch",
			repoPath, current, baseBranch)
	}

	if _, err := runGit(ctx, repoPath, "merge", "--ff-only", commit); err != nil {
		// Either the base moved independently or the tree is dirty. Both are
		// human decisions: silently picking a side is how work disappears.
		return fmt.Errorf("cannot fast-forward %s to %s: %w", baseBranch, short(commit), err)
	}
	return nil
}

func verifyCommit(ctx context.Context, repoPath, commit string) error {
	typ, err := runGit(ctx, repoPath, "cat-file", "-t", commit)
	if err != nil {
		return fmt.Errorf("commit %s is not in this repository: %w", short(commit), err)
	}
	if typ != "commit" {
		return fmt.Errorf("%s is a %s, not a commit", short(commit), typ)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func short(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}

// MergeInto brings commit into the worktree at path.
//
// Not --ff-only, unlike base-branch integration: a role's branch has its own
// commits, so a hand-off is usually a real merge. A conflict is left in the
// tree on purpose — the agent is told the merge did not complete and resolves
// it in the place it happened, which is the only place it can be resolved.
func (Git) MergeInto(ctx context.Context, worktreePath, commit string) error {
	// Already an ancestor means nothing to do. Checking is cheaper than
	// merging, and it keeps a resumed lease from producing an empty commit.
	if _, err := runGit(ctx, worktreePath, "merge-base", "--is-ancestor", commit, "HEAD"); err == nil {
		return nil
	}
	if _, err := runGit(ctx, worktreePath, "merge", "--no-edit", commit); err != nil {
		return fmt.Errorf("merging %s into %s: %w", commit, worktreePath, err)
	}
	return nil
}

// Resolve expands a commit-ish to the sha it names in the tree at path.
//
// ^{commit} makes the failure explicit rather than clever: a tag or a tree
// that is not a commit errors here instead of producing an object id that
// every later step treats as a commit and fails on obscurely.
func (Git) Resolve(ctx context.Context, worktreePath, ref string) (string, error) {
	out, err := runGit(ctx, worktreePath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("%q resolved to nothing in %s", ref, worktreePath)
	}
	return sha, nil
}
