// Package hatchery manages a project's worktrees.
//
// One linked worktree per role: a single repository and object store, so a
// peer's commit resolves without a fetch, and roles that cannot overwrite each
// other's files.
package hatchery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// zerg's own identity, for zerg's own commit.
	//
	// This is the one commit the orchestrator authors rather than an agent, and
	// it happens while bootstrapping a repository that may have nothing in it —
	// including, on a fresh machine or a CI runner, no configured user.name.
	// git then refuses with "Please tell me who you are", and a project fails
	// to open for a reason that has nothing to do with the project.
	//
	// Passed with -c rather than written to the config: it applies to this
	// invocation only, so nothing is left behind in a repository someone else
	// owns, and an operator who does have an identity keeps it for every commit
	// that is actually theirs.
	if _, err := git(ctx, h.repoPath,
		"-c", "user.name=zerg", "-c", "user.email=zerg@localhost",
		"commit", "-q", "--allow-empty", "-m", "Initial commit"); err != nil {
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

// PreviewDir is the worktree a local preview runs in.
//
// One per project, reused and checked out detached at whatever commit is being
// previewed. Its own worktree rather than the operator's checkout for two
// reasons: running a build in the directory somebody is working in is rude,
// and a detached checkout can show a commit that has not merged, which is the
// whole point of previewing at the approval gate.
const PreviewDir = "preview"

// PreviewWorktree checks a commit out into the preview worktree and returns
// its path.
//
// Detached on purpose: this is a copy to run, not a branch to work on, and a
// branch here would be a thing that could drift, be committed to, or be
// confused with a role's.
func (h *Hatchery) PreviewWorktree(ctx context.Context, commit string) (string, error) {
	path := h.Path(PreviewDir)

	// The same rule the role worktrees need, for the same reason and one more:
	// a preview can be the first worktree a project ever gets, if somebody runs
	// a change before starting a swarm. Without it .worktrees/preview is an
	// untracked directory in the operator's own `git status`, and an agent
	// running `git add -A` commits an embedded repository into the project.
	if err := h.ensureIgnored(ctx); err != nil {
		return "", err
	}

	if isWorktree(ctx, path) {
		// Reused rather than recreated: a rebuild between two commits should
		// not throw away a node_modules or a target directory that the build
		// is about to want again.
		if _, err := git(ctx, path, "checkout", "--detach", "--force", commit); err != nil {
			return "", fmt.Errorf("checking out %s to preview: %w", short(commit), err)
		}
		return path, nil
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists and is not a git worktree; move it aside", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if _, err := git(ctx, h.repoPath, "worktree", "add", "--force", "--detach", path, commit); err != nil {
		return "", fmt.Errorf("creating the preview worktree at %s: %w", short(commit), err)
	}
	return path, nil
}

// short is a commit as a person would write it.
func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
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

	// Added and Removed are the line counts, which git reports without reading
	// the file. They are what a list of files can show before anything is
	// loaded.
	Added   int `json:"added"`
	Removed int `json:"removed"`

	// Binary is a file git will not diff: an image, a font, a compiled thing.
	// Said rather than fetched. Its bytes are not text, and reading them into a
	// string to send to a browser is how a review of a change to a PNG became
	// a megabyte of mojibake.
	Binary bool `json:"binary,omitempty"`

	// TooLarge is a file past the byte cap. Also said rather than left empty:
	// a file with no content and no diff is otherwise indistinguishable from a
	// file that did not change, which is the wrong thing to tell a reviewer.
	TooLarge bool `json:"tooLarge,omitempty"`

	// Deferred is a file whose content was not read because the diff is large.
	// Every changed file is listed; the ones past the eager limit are fetched
	// when they are opened.
	Deferred bool `json:"deferred,omitempty"`
}

// ChangedFiles returns what a commit touched, with the content of each file as
// of that commit.
//
// Content as well as diff, because the two answer different questions. A diff
// is right for a change to existing code. For a file the commit created — a
// spec, a design note — the diff is the document with a plus in front of every
// line, and the thing you actually want to read is the document.
func (h *Hatchery) ChangedFiles(ctx context.Context, sha string, maxBytes, eager int) ([]ChangedFile, error) {
	return h.changed(ctx, sha, "", maxBytes, eager)
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
func (h *Hatchery) RangeFiles(ctx context.Context, base, sha string, maxBytes, eager int) ([]ChangedFile, error) {
	return h.changed(ctx, sha, base, maxBytes, eager)
}

// lineStats asks git how much each file changed, and which files it will not
// diff at all.
//
// --numstat answers both without reading a file: counts per path, and a pair of
// dashes where the file is binary. That is how a listing can show the shape of
// a change before any of it is loaded.
func (h *Hatchery) lineStats(ctx context.Context, sha, base string) (map[string]struct {
	added, removed int
	binary         bool
}, error) {
	var out string
	var err error
	if base != "" {
		out, err = git(ctx, h.repoPath, "diff", "--numstat", "-z", base+"..."+sha)
	} else {
		out, err = git(ctx, h.repoPath, "show", "--format=", "--numstat", "-z", sha)
	}
	if err != nil {
		return nil, fmt.Errorf("counting what %s changed: %w", sha, err)
	}
	stats := map[string]struct {
		added, removed int
		binary         bool
	}{}
	// -z for the same reason as the listing: a path with a space in it, and a
	// rename, which the line format writes as "old => new" and no lookup ever
	// matched, so every renamed file reported no lines changed.
	//
	// The record is "added\tremoved\tpath" in one NUL-terminated field, except
	// for a rename, where the path is empty and the old and new names follow as
	// two fields of their own.
	for i, fields := 0, strings.Split(out, "\x00"); i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) < 3 {
			continue
		}
		path := parts[2]
		if path == "" {
			if i+2 >= len(fields) {
				break
			}
			path = fields[i+2]
			i += 2
		}
		if parts[0] == "-" && parts[1] == "-" {
			stats[path] = struct {
				added, removed int
				binary         bool
			}{binary: true}
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		stats[path] = struct {
			added, removed int
			binary         bool
		}{added: added, removed: removed}
	}
	return stats, nil
}

// ErrNoSuchRevision means git could not resolve a ref this repository was asked
// about: a branch deleted, a worktree pruned, a clone that never had it.
//
// A distinct error because it is the operator's problem rather than the
// daemon's, and the two must not be answered the same way. Everything else that
// can fail here (git missing from PATH, an unreadable repository, a cancelled
// request) really is a server fault, and telling someone their commit does not
// exist when git itself is not installed sends them looking in the wrong place.
var ErrNoSuchRevision = errors.New("no such revision")

// resolves reports whether git can turn a ref into a commit in this repository.
//
// Asked explicitly rather than inferred from the failure of the command that
// wanted it: matching on git's stderr means matching on prose that changes
// between versions and locales, and `rev-parse --verify` answers exactly this
// question with an exit code.
//
// The exit code is the whole point, so this does not go through git() above,
// which collapses every failure into one message. `--quiet` makes an unknown
// ref exit 1 and print nothing; anything else, including exit 128 for "not a
// git repository", is an operational failure and is returned as one. Without
// that distinction a broken repository reports every commit as missing.
func (h *Hatchery) resolves(ctx context.Context, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = h.repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0 {
			return false, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Errorf("resolving %s in %s: %s", ref, h.repoPath, msg)
	}
	return true, nil
}

// load fills in one file's content and diff, or says why it did not.
func (h *Hatchery) load(ctx context.Context, f *ChangedFile, sha, base string, maxBytes int) {
	// Only for the files the cockpit renders as documents.
	//
	// It used to read every file's full content as well as its diff, which is
	// two git processes per file: a thirty-file listing spawned sixty of them
	// and measured 840ms to answer, for 1.4KB of JSON. Nothing ever displayed
	// the content of a .rs or a .go -- those are read as diffs -- so the bytes
	// were fetched, serialised, sent, and dropped.
	if f.Status != "D" && isDoc(f.Path) {
		if body, err := git(ctx, h.repoPath, "show", sha+":"+f.Path); err == nil {
			if maxBytes <= 0 || len(body) <= maxBytes {
				f.Content = body
			} else {
				f.TooLarge = true
			}
		}
	}
	// The diff comes too: content alone cannot show what changed in a file
	// that already existed.
	var d string
	var err error
	if base != "" {
		d, err = git(ctx, h.repoPath, "diff", base+"..."+sha, "--", f.Path)
	} else {
		d, err = git(ctx, h.repoPath, "show", "--format=", "--patch", sha, "--", f.Path)
	}
	if err != nil {
		return
	}
	if maxBytes <= 0 || len(d) <= maxBytes {
		f.Diff = d
		return
	}
	f.TooLarge = true
}

// isDoc reports whether a file is one the cockpit renders as a document
// rather than as a diff. It mirrors the same test in Attention.vue: a spec is
// the deliverable at a planner's gate, and its diff is the document with a
// plus in front of every line.
func isDoc(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}

// patches asks git for the diff of many files at once.
//
// One process rather than one per file. Answering a thirty-file listing took
// 840ms, nearly all of it spent starting git over and over; batched, the same
// answer is one invocation.
//
// The sections are matched to the paths by order rather than by parsing the
// "diff --git a/x b/x" line, because that line quotes and escapes anything
// unusual in a path and re-parsing it is how the listing got a file called
// "b.txt" out of "a b.txt". Git emits sections in the order it was given, which
// is the order this list already has. If the counts disagree, the caller is
// told nothing came back and falls back to reading each file on its own: a
// slower answer is better than a diff shown against the wrong name.
func (h *Hatchery) patches(ctx context.Context, sha, base string, paths []string) map[string]string {
	if len(paths) == 0 {
		return nil
	}
	args := []string{"diff", base + "..." + sha, "--"}
	if base == "" {
		args = []string{"show", "--format=", "--patch", sha, "--"}
	}
	out, err := git(ctx, h.repoPath, append(args, paths...)...)
	if err != nil {
		return nil
	}

	const marker = "diff --git "
	var sections []string
	for _, chunk := range strings.Split(out, "\n"+marker) {
		if chunk == "" {
			continue
		}
		if !strings.HasPrefix(chunk, marker) {
			chunk = marker + chunk
		}
		sections = append(sections, chunk)
	}
	if len(sections) != len(paths) {
		return nil
	}
	by := make(map[string]string, len(paths))
	for i, p := range paths {
		by[p] = strings.TrimRight(sections[i], "\n") + "\n"
	}
	return by
}

// LoadFile reads one file of a change, for the ones a listing left alone.
func (h *Hatchery) LoadFile(ctx context.Context, base, sha, path string, maxBytes int) (*ChangedFile, error) {
	files, err := h.changed(ctx, sha, base, maxBytes, 0)
	if err != nil {
		return nil, err
	}
	for i := range files {
		if files[i].Path != path {
			continue
		}
		f := files[i]
		if f.Binary {
			return &f, nil
		}
		f.Deferred = false
		h.load(ctx, &f, sha, base, maxBytes)
		return &f, nil
	}
	return nil, fmt.Errorf("%q is not in this change: %w", path, ErrNoSuchRevision)
}

// changed lists what a commit touched.
//
// eager bounds how many of those files are read in full; the rest are listed
// and fetched when they are opened. Zero reads none of them, which is what
// LoadFile wants: it needs the list to find one file, not the contents of the
// other ninety-nine. Negative reads them all.
func (h *Hatchery) changed(ctx context.Context, sha, base string, maxBytes, eager int) ([]ChangedFile, error) {
	// Checked before the diff, so a ref this repository does not have is
	// reported as that rather than as whatever the diff command said about it.
	for _, ref := range []struct{ what, ref string }{{"", sha}, {"base branch ", base}} {
		if ref.ref == "" {
			continue
		}
		ok, err := h.resolves(ctx, ref.ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s%s is not in %s",
				ErrNoSuchRevision, ref.what, ref.ref, h.repoPath)
		}
	}

	var out string
	var err error
	if base != "" {
		out, err = git(ctx, h.repoPath, "diff", "--name-status", "-z", base+"..."+sha)
	} else {
		out, err = git(ctx, h.repoPath, "show", "--format=", "--name-status", "-z", sha)
	}
	if err != nil {
		return nil, fmt.Errorf("listing what %s changed: %w", sha, err)
	}

	stats, err := h.lineStats(ctx, sha, base)
	if err != nil {
		return nil, err
	}

	var files []ChangedFile
	var read []int // the ones to fill in, by index
	for _, rec := range nameStatus(out) {
		f := ChangedFile{Status: rec.status, Path: rec.path}
		if st, ok := stats[f.Path]; ok {
			f.Added, f.Removed, f.Binary = st.added, st.removed, st.binary
		}

		// A binary file is named and counted and not read. There is nothing in
		// it a reviewer can read in a browser, and its bytes are not a string.
		if f.Binary {
			files = append(files, f)
			continue
		}
		// Past the eager limit the file is listed and left to be opened. A
		// hundred-file change otherwise reads every file, and its diff, before
		// anyone has looked at the first one.
		if eager >= 0 && len(read) >= eager {
			f.Deferred = true
			files = append(files, f)
			continue
		}
		read = append(read, len(files))
		files = append(files, f)
	}

	paths := make([]string, len(read))
	for i, idx := range read {
		paths[i] = files[idx].Path
	}
	batch := h.patches(ctx, sha, base, paths)
	for _, idx := range read {
		f := &files[idx]
		d, ok := batch[f.Path]
		if !ok {
			// The batch could not be matched to its paths; read this one on
			// its own rather than show it as unchanged.
			h.load(ctx, f, sha, base, maxBytes)
			continue
		}
		if maxBytes > 0 && len(d) > maxBytes {
			f.TooLarge = true
		} else {
			f.Diff = d
		}
		if isDoc(f.Path) && f.Status != "D" && !f.TooLarge {
			h.load(ctx, f, sha, base, maxBytes)
		}
	}
	return files, nil
}

// nameStatus reads `git diff --name-status -z` into a status and a path each.
//
// -z rather than the default line format, and not whitespace-delimited. Split
// on whitespace, "M\tsrc/a b.txt" gave the path "b.txt": a name no file has, so
// its counts were missing, its content never loaded, and opening it asked the
// daemon for a path that does not exist. The default output also C-quotes any
// path holding a byte outside ASCII, so café.txt arrived as "caf\303\251.txt".
// With -z git separates every field with a NUL and quotes nothing.
//
// A rename or a copy carries two paths, old then new. The new one is the file
// that exists now and the only one a reader can open, so that is the one kept.
func nameStatus(out string) []struct{ status, path string } {
	fields := strings.Split(out, "\x00")
	var recs []struct{ status, path string }
	for i := 0; i < len(fields); i++ {
		st := fields[i]
		if st == "" {
			continue
		}
		paths := 1
		if st[0] == 'R' || st[0] == 'C' {
			paths = 2
		}
		if i+paths >= len(fields) {
			break
		}
		recs = append(recs, struct{ status, path string }{st[:1], fields[i+paths]})
		i += paths
	}
	return recs
}

// Workspace is what the worktrees cost, for a header that would otherwise be
// asking you to guess.
type Workspace struct {
	// Worktrees is how many exist on disk, which is not the same as how many
	// roles are enabled: a role removed from the team leaves its checkout.
	Worktrees []WorktreeInfo `json:"worktrees"`
	Bytes     int64          `json:"bytes"`
}

type WorktreeInfo struct {
	Role  string `json:"role"`
	Bytes int64  `json:"bytes"`
}

// Measure reports the worktrees on disk and what they occupy.
//
// Best effort throughout, like dirSize: this is a figure in a header, and a
// permissions problem in one checkout should understate a number rather than
// fail the page it appears on.
func (h *Hatchery) Measure() Workspace {
	var out Workspace
	entries, err := os.ReadDir(filepath.Join(h.repoPath, WorktreesDir))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := dirSize(filepath.Join(h.repoPath, WorktreesDir, e.Name()))
		out.Worktrees = append(out.Worktrees, WorktreeInfo{Role: e.Name(), Bytes: n})
		out.Bytes += n
	}
	return out
}

// ── mergeability ──────────────────────────────────────────────────────────

// Mergeable is whether the work would land, and what stands in the way.
type Mergeable struct {
	// Clean is whether the merge would go through without a person resolving
	// anything.
	Clean bool `json:"clean"`
	// Conflicts are the paths git could not merge, when it could not.
	Conflicts []string `json:"conflicts,omitempty"`
	// BaseAhead is how many commits the base branch has taken since this work
	// diverged from it. A diff that was right against yesterday's base is the
	// usual reason an approval that looked clean fails at the merge.
	BaseAhead int `json:"baseAhead"`
	// Diverged is set when the landing is a fast-forward and this commit is no
	// longer on top of the base. The merge itself may be perfectly clean and
	// the integration will still refuse: `git merge --ff-only` is what runs,
	// and it declines anything that is not already ahead. Reported separately
	// from a conflict because what fixes it is different: rebase the work, not
	// resolve a file.
	Diverged bool `json:"diverged,omitempty"`
}

// MergeCheck asks whether a commit still merges into the base branch.
//
// `git merge-tree --write-tree` does the merge in memory: no worktree, no
// index, nothing checked out and nothing to clean up if it fails. Exit 0 means
// clean and prints the tree it would produce; exit 1 means conflicts, and the
// lines after the tree name every path that has one, tab-separated, once per
// stage. Anything else is a real failure and is reported as one.
//
// Verified against a repository with a base that had moved underneath: clean
// returns a tree id alone, conflicting returns the tree, three staged entries
// for the one file, then git's own account of what it could not merge.
// ffOnly says the landing is `git merge --ff-only`, which is what the merge
// integration mode runs. A pull request is merged by the forge with an
// ordinary three-way merge, and branch mode lands nothing at all, so both of
// those are answered by the merge test alone.
func (h *Hatchery) MergeCheck(ctx context.Context, base, commit string, ffOnly bool) (*Mergeable, error) {
	for _, ref := range []string{base, commit} {
		ok, err := h.resolves(ctx, ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%q: %w", ref, ErrNoSuchRevision)
		}
	}

	out := &Mergeable{}
	// How far the base has moved since the work left it. Counted separately
	// from the merge because a base that has moved is worth saying even when
	// the merge is still clean: it is the difference between reviewing what
	// will land and reviewing what was written.
	if ahead, err := git(ctx, h.repoPath, "rev-list", "--count", commit+".."+base); err == nil {
		out.BaseAhead, _ = strconv.Atoi(ahead)
	}

	// Answering the question the integration will actually ask. merge-tree
	// performs a three-way merge, so a commit that had diverged from a base it
	// did not conflict with was reported as "merges cleanly" and then refused
	// at the gate with "cannot fast-forward": the check and the landing
	// disagreed, and the reader was told the wrong one.
	if ffOnly {
		if _, err := git(ctx, h.repoPath, "merge-base", "--is-ancestor", base, commit); err != nil {
			out.Diverged = true
			return out, nil
		}
		out.Clean = true
		return out, nil
	}

	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", base, commit)
	cmd.Dir = h.repoPath
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		out.Clean = true
		return out, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git merge-tree %s %s: %s", base, commit, msg)
	}

	out.Conflicts = conflictPaths(stdout.String())
	return out, nil
}

// conflictPaths reads the paths out of merge-tree's conflicted output.
//
// The first line is the tree it would have written. What follows, until a blank
// line, is one entry per conflicting stage: "<mode> <oid> <stage>\t<path>", so a
// single conflicted file appears three times. Git's prose about what it could
// not merge comes after that blank line and is not parsed: the paths are the
// part a caller can act on.
func conflictPaths(out string) []string {
	var paths []string
	seen := map[string]bool{}
	for i, line := range strings.Split(out, "\n") {
		if i == 0 {
			continue // the tree id
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		_, path, ok := strings.Cut(line, "\t")
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}
