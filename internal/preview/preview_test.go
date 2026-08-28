package preview

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/store"
)

// repo builds a git repository with one commit and returns it with the sha.
func repo(t *testing.T, files map[string]string) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-qm", "one")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

func fixture(t *testing.T, files map[string]string) (*Manager, *store.DB, *store.Project, string) {
	t.Helper()
	ctx := context.Background()
	dir, commit := repo(t, files)

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Seed(ctx, db, "claude"); err != nil {
		t.Fatal(err)
	}
	p, err := db.CreateProject(ctx, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(db, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	t.Cleanup(func() { m.StopAll(context.Background()) })
	return m, db, p, commit
}

// The whole point: a commit that has not merged, running, on a port the daemon
// picked, in a worktree of its own.
func TestAPreviewRunsTheCommitOnThePortItWasGiven(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		// Reads $PORT, which is the contract, and serves the tree it is in.
		"serve.sh":   "#!/bin/sh\nexec python3 -m http.server \"$PORT\" --bind 127.0.0.1\n",
		"index.html": "<h1>the commit under review</h1>",
	})

	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", ReadySecs: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := m.Start(ctx, p, target, commit, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if a.Kind != store.ArtifactService || a.Port == 0 {
		t.Fatalf("artifact = %+v", a)
	}
	// The daemon's, so the swarm going down leaves it alone.
	if a.Owner != store.OwnerDaemon {
		t.Errorf("owner = %q, want daemon", a.Owner)
	}

	// It is actually serving the commit's files.
	resp, err := http.Get("http://127.0.0.1:" + itoa(a.Port) + "/index.html")
	if err != nil {
		t.Fatalf("the preview did not answer: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "the commit under review") {
		t.Errorf("served %q", body)
	}

	// In its own worktree, not the operator's checkout.
	if strings.Contains(string(body), "") && !dirExists(filepath.Join(p.Path, ".worktrees", "preview")) {
		t.Error("no preview worktree; the build ran somewhere unexpected")
	}

	// Stopping takes the process with it, and the row says so.
	m.Stop(ctx, p.ID)
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(a.Port), 300*time.Millisecond); err == nil {
		t.Error("something is still listening after Stop")
	}
	after, err := db.GetArtifact(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Live() {
		t.Error("the row still says the service is live")
	}
}

// A command that fails is the ordinary case, and the reason is in its output.
// A message that says only "it did not come up" sends somebody to the daemon's
// log to find out what a command they wrote printed.
func TestAFailedStartCarriesItsOutput(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		"serve.sh": "#!/bin/sh\necho 'Error: cannot find module ./app'\nexit 1\n",
	})
	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", ReadySecs: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Start(ctx, p, target, commit, "")
	if err == nil {
		t.Fatal("a command that exited immediately was reported as up")
	}
	var failed *StartError
	if !asStartError(err, &failed) {
		t.Fatalf("error is %T, want a StartError carrying the log", err)
	}
	if !strings.Contains(failed.Log, "cannot find module") {
		t.Errorf("the log did not travel with the failure: %q", failed.Log)
	}
	// It exited rather than timed out, and the message should say that: waiting
	// the full ten seconds to say "nothing answered" tells nobody anything.
	if !strings.Contains(failed.Reason, "exited") {
		t.Errorf("reason = %q", failed.Reason)
	}
	// Nothing was recorded as running.
	live, _ := db.LiveServices(ctx, p.ID)
	if len(live) != 0 {
		t.Errorf("%d services recorded after a failed start", len(live))
	}
}

// Starting a second preview replaces the first: two builds of one project are
// two things fighting for the same disk, and the question is always about the
// commit in front of you.
func TestStartingAgainReplacesTheRunningPreview(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		"serve.sh": "#!/bin/sh\nexec python3 -m http.server \"$PORT\" --bind 127.0.0.1\n",
	})
	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", ReadySecs: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := m.Start(ctx, p, target, commit, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(ctx, p, target, commit, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == second.Port {
		t.Error("the second preview took the first one's port; the first cannot have stopped")
	}
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(first.Port), 300*time.Millisecond); err == nil {
		t.Error("the first preview is still running")
	}

	live, err := db.LiveServices(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != second.ID {
		t.Errorf("%d live services, want only the second", len(live))
	}
}

// A shell exiting leaves what it started running, still holding the port. The
// process group is what makes stopping mean stopping.
func TestStoppingKillsWhatTheCommandStarted(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		// The shell starts a server and waits: killing the shell alone leaves
		// python holding the port.
		"serve.sh": "#!/bin/sh\npython3 -m http.server \"$PORT\" --bind 127.0.0.1 &\nwait\n",
	})
	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", ReadySecs: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := m.Start(ctx, p, target, commit, "")
	if err != nil {
		t.Fatal(err)
	}
	m.Stop(ctx, p.ID)

	// Give the group a moment to go.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(a.Port), 200*time.Millisecond); err != nil {
			return // gone, which is the point
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("the child the command started is still holding the port")
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func asStartError(err error, target **StartError) bool {
	for err != nil {
		if e, ok := err.(*StartError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Killing the process group is not the whole story for everything. `docker
// compose up` interrupted stops its containers and leaves them exited, one set
// per preview, and only the command knows they exist.
func TestTheStopCommandRunsBeforeTheGroupIsKilled(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		"serve.sh": "#!/bin/sh\nexec python3 -m http.server \"$PORT\" --bind 127.0.0.1\n",
	})
	// A stand-in for `docker compose down`: it leaves proof it ran.
	marker := filepath.Join(t.TempDir(), "cleaned")
	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", StopCommand: "echo cleaned up > " + marker, ReadySecs: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Start(ctx, p, target, commit, ""); err != nil {
		t.Fatal(err)
	}
	m.Stop(ctx, p.ID)

	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the stop command did not run: %v", err)
	}
	if !strings.Contains(string(body), "cleaned up") {
		t.Errorf("marker = %q", body)
	}
}

// A preview is a checkout of a commit, so anything in .gitignore is not in it:
// compose that wants an env file fails on the first run for every project that
// has one, which is a wall rather than a message.
func TestFilesGitDoesNotCarryAreBroughtIn(t *testing.T) {
	ctx := context.Background()
	m, db, p, commit := fixture(t, map[string]string{
		".gitignore": ".env\n",
		// Serves only if the file it needs arrived.
		"serve.sh": "#!/bin/sh\ntest -f .env || { echo 'no .env here'; exit 1; }\n" +
			"exec python3 -m http.server \"$PORT\" --bind 127.0.0.1\n",
	})
	// Untracked in the operator's checkout, exactly like a real one.
	if err := os.WriteFile(filepath.Join(p.Path, ".env"), []byte("API_URL=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := db.SaveDeployTarget(ctx, &store.DeployTarget{
		ProjectID: p.ID, Name: "preview", Kind: store.TargetLocal,
		Command: "./serve.sh", ReadySecs: 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without being asked to bring it: the command fails, and says why.
	if _, err := m.Start(ctx, p, target, commit, ""); err == nil {
		t.Fatal("started without the file it needs")
	}

	target.CopyFiles = ".env"
	if _, err := db.SaveDeployTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, p, target, commit, ""); err != nil {
		t.Fatalf("Start with the file brought in: %v", err)
	}

	// Copied, and no more readable than the original: a .env that arrives
	// world-readable is worse than one that does not arrive.
	copied := filepath.Join(p.Path, ".worktrees", "preview", ".env")
	info, err := os.Stat(copied)
	if err != nil {
		t.Fatalf("the file was not brought in: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("copied with mode %v, want the original's 0600", info.Mode().Perm())
	}
}

// A target is configuration, and configuration that can name /etc/shadow is a
// way to read it.
func TestCopyingRefusesPathsOutsideTheProject(t *testing.T) {
	for _, bad := range []string{"/etc/hosts", "../../.ssh/id_rsa", "sub/../../escape"} {
		if err := copyUntracked(t.TempDir(), t.TempDir(), bad); err == nil {
			t.Errorf("copying %q was allowed", bad)
		}
	}
}
