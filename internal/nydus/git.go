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

// TaskBranchPrefix names the branch a task's finished work is published on.
//
// One branch per task rather than reusing the role's, because a pull request
// should be about one task. A role branch accumulates everything that role has
// ever done, and a reviewer opening the PR would be reading work that landed
// weeks ago.
const TaskBranchPrefix = "zerg/"

// Publish pushes a branch at commit and opens a pull request for it.
//
// The description is the terminal role's handoff note, which is already an
// account of what was done and what was checked — written for the next reader,
// which is exactly who opens a PR.
//
// Shelling out to gh rather than talking to the API: gh already holds the
// credentials, already knows which remote is which, and adding an HTTP client
// and a token store here would duplicate all of it worse.
func (Git) Publish(ctx context.Context, repoPath, base, commit, title, body string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf(
			"opening a pull request needs the gh CLI, which is not installed (brew install gh)")
	}
	remote, err := runGit(ctx, repoPath, "remote")
	if err != nil || strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf(
			"this repository has no remote, so there is nowhere to open a pull request; " +
				"add one, or set integration to merge or branch")
	}

	branch := TaskBranchPrefix + slug(title)
	// -f so a retried completion updates the branch rather than failing on a
	// name that already exists.
	if _, err := runGit(ctx, repoPath, "branch", "-f", branch, commit); err != nil {
		return "", fmt.Errorf("creating %s: %w", branch, err)
	}
	if _, err := runGit(ctx, repoPath, "push", "-u", "--force-with-lease",
		strings.Fields(remote)[0], branch); err != nil {
		return "", fmt.Errorf("pushing %s: %w", branch, err)
	}

	out, err := exec.CommandContext(ctx, "gh", "pr", "create",
		"--base", base, "--head", branch, "--title", title, "--body", body,
	).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// A pull request that already exists is a retry, not a failure.
		if strings.Contains(msg, "already exists") {
			if url, e := existingPR(ctx, repoPath, branch); e == nil {
				return url, nil
			}
		}
		return "", fmt.Errorf("gh pr create: %v (%s)", err, msg)
	}
	return firstURL(string(out)), nil
}

func existingPR(ctx context.Context, repoPath, branch string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch, "--json", "url", "-q", ".url")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// firstURL pulls the pull request link out of gh's output, which prints it on
// its own line among other chatter.
func firstURL(s string) string {
	for _, line := range strings.Fields(s) {
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return strings.TrimSpace(s)
}

// slug turns a task name into something git will accept as a branch.
func slug(name string) string {
	var b strings.Builder
	lastDash := true // no leading dash
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
