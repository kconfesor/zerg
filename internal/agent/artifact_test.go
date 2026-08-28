package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

// task queues one card for the first role, which is what these tests need to
// have something for an artifact to belong to.
func (f *fixture) task(t *testing.T, name string) *store.Task {
	t.Helper()
	task, err := f.nyd.NewTask(context.Background(), f.project.ID, name, "build it")
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	return task
}

// A file an agent produced is kept where the worktree cannot take it with it.
func TestAddingAFileKeepsItAndNamesTheCard(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")

	// The role is holding the card, which is how the artifact finds it without
	// the agent having to say.
	c := f.client(t, "coder")
	if _, err := c.Next(ctx, 0); err != nil {
		t.Fatalf("Next: %v", err)
	}

	report := filepath.Join(f.project.Path, "coverage.html")
	if err := os.WriteFile(report, []byte("<h1>92%</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	made, err := c.Artifact(ctx, ArtifactArgs{Kind: "file", Path: "coverage.html", Label: "Coverage"})
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if made.Kind != store.ArtifactFile || made.Label != "Coverage" {
		t.Errorf("stored %+v", made)
	}
	if made.TaskID == nil || *made.TaskID != task.ID {
		t.Errorf("artifact landed on %v, want the card the role is holding (%s)", made.TaskID, task.ID)
	}
	if made.Role != "coder" {
		t.Errorf("role = %q, want the one the token proves", made.Role)
	}
	if made.Bytes != 12 || !strings.HasPrefix(made.MIME, "text/html") {
		t.Errorf("recorded %d bytes of %q", made.Bytes, made.MIME)
	}

	// The bytes are in the store, so deleting the working copy loses nothing.
	if err := os.Remove(report); err != nil {
		t.Fatal(err)
	}
	blob, err := f.blobs.Open(made.SHA256)
	if err != nil {
		t.Fatalf("the bytes did not survive: %v", err)
	}
	blob.Close()

	// And the card lists it.
	list, err := f.db.ArtifactsForTask(ctx, task.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ArtifactsForTask = %d (%v)", len(list), err)
	}
}

// An image is its own kind, because it is the one thing the cockpit shows
// without being asked to.
func TestAnImageIsRecordedAsOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")
	c := f.client(t, "coder")

	shot := filepath.Join(f.project.Path, "login.png")
	if err := os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nrest of it"), 0o644); err != nil {
		t.Fatal(err)
	}
	made, err := c.Artifact(ctx, ArtifactArgs{Kind: "file", Path: shot})
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if made.Kind != store.ArtifactImage {
		t.Errorf("kind = %q, want image", made.Kind)
	}
	// With no label, the file's own name is the label rather than nothing.
	if made.Label != "login.png" || made.Name != "login.png" {
		t.Errorf("label %q, name %q", made.Label, made.Name)
	}
}

// An artifact is something the work produced. Without this rule, one line in a
// poisoned file an agent read -- `zerg artifact add ~/.ssh/id_rsa` -- copies a
// key into the store and publishes it on an HTTP surface with no
// authentication.
func TestAFileOutsideTheProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")
	c := f.client(t, "coder")

	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := c.Artifact(ctx, ArtifactArgs{Kind: "file", Path: outside})
	if err == nil {
		t.Fatal("a file outside the project was stored")
	}
	if !strings.Contains(err.Error(), "outside the project") {
		t.Errorf("error was %q, which does not say what the rule is", err)
	}

	// And the same by relative escape, which is the form that arrives by
	// accident rather than on purpose.
	if _, err := c.Artifact(ctx, ArtifactArgs{Kind: "file", Path: "../../etc/hosts"}); err == nil {
		t.Error("a relative path out of the project was stored")
	}
}

// A symlink inside the tree pointing out of it is the way around the rule
// above, so the check resolves links before comparing.
func TestASymlinkOutOfTheProjectIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.task(t, "Calculator")
	c := f.client(t, "coder")

	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.project.Path, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := c.Artifact(ctx, ArtifactArgs{Kind: "file", Path: "innocent.txt"}); err == nil {
		t.Error("a symlink out of the project was followed and stored")
	}
}

// A service is a port something is on. Registering one nothing is listening on
// produces a link that fails only when somebody clicks it.
func TestRegisteringAServiceChecksSomethingIsThere(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	task := f.task(t, "Calculator")
	c := f.client(t, "coder")
	if _, err := c.Next(ctx, 0); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	made, err := c.Artifact(ctx, ArtifactArgs{Kind: "service", Port: port, Label: "Dev server"})
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if made.Kind != store.ArtifactService || made.Port != port {
		t.Errorf("stored %+v", made)
	}
	if !made.Live() {
		t.Error("a service just registered is not live")
	}
	if made.TaskID == nil || *made.TaskID != task.ID {
		t.Errorf("service landed on %v, want %s", made.TaskID, task.ID)
	}

	// Nothing on the port: refused, and the message says what to do.
	ln.Close()
	if _, err := c.Artifact(ctx, ArtifactArgs{Kind: "service", Port: port}); err == nil {
		t.Error("a port with nothing on it was registered")
	}

	// The swarm going down takes the service with it: the process holding the
	// port is gone, and the row must stop offering a link to whatever binds
	// that port next.
	if n, err := f.db.StopServices(ctx, f.project.ID); err != nil || n != 1 {
		t.Errorf("StopServices stopped %d (%v), want 1", n, err)
	}
	after, err := f.db.GetArtifact(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Live() {
		t.Error("the service is still live after the swarm stopped")
	}
}
