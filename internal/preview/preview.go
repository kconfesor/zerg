// Package preview runs a project's own code so a person can click it.
//
// The third of the three ways work becomes something to look at. An agent's
// dev server (`zerg artifact serve`) is a process the agent started and the
// swarm owns; a remote deployment sends the work elsewhere. This is in
// between: the daemon checks a commit out, runs the project's own command, and
// proxies the result.
//
// That ordering matters at the approval gate. The commit being decided about
// has not merged, and may never; a preview of it is the only way to answer
// "what does it look like" before answering "should this land".
package preview

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kconfesor/zerg/internal/hatchery"
	"github.com/kconfesor/zerg/internal/store"
)

// logTail is how much of a preview's output is kept.
//
// Enough to hold a stack trace or a compose failure, bounded because a build
// that loops printing warnings would otherwise be held in memory until the
// daemon stops.
const logTail = 256 << 10 // 256 KiB

// dialEvery is how often a starting preview is checked for life.
const dialEvery = 500 * time.Millisecond

// Manager owns the running previews, one per project.
//
// One, not many: a second preview of the same project is two builds competing
// for the same disk and the same ports, and the question it answers is always
// about the commit in front of you. Starting one stops the last.
type Manager struct {
	db    *store.DB
	log   *slog.Logger
	state string

	mu      sync.Mutex
	running map[string]*run
}

type run struct {
	artifact string
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	log      *ring
	done     chan struct{}

	// What undoes the command, and where to run it; see stop_command.
	stopCmd string
	dir     string
}

func NewManager(db *store.DB, log *slog.Logger, stateDir string) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{db: db, log: log, state: stateDir, running: map[string]*run{}}
}

// Start runs a target at a commit and returns the service artifact once the
// port answers.
//
// Synchronous to the point of readiness, because the caller is a person who
// pressed a button and the two outcomes they care about -- it is up, or it
// failed and here is why -- are both known by then. A build that takes three
// minutes is bounded by the target's own ready_secs.
func (m *Manager) Start(ctx context.Context, project *store.Project, target *store.DeployTarget,
	commit, taskID string) (*store.Artifact, error) {
	if target.Kind != store.TargetLocal {
		return nil, fmt.Errorf("%q is a remote target; this runs local ones", target.Name)
	}

	// The old one first: its port, its containers and its worktree are all
	// about to be reused.
	m.Stop(ctx, project.ID)

	hat := hatchery.New(project.Path)
	dir, err := hat.PreviewWorktree(ctx, commit)
	if err != nil {
		return nil, err
	}
	// Whatever git does not track and the command still needs. A worktree is
	// made from a commit, so .env is not in it, and `docker compose up` fails
	// on the first run with "env file .env not found" for every project that
	// has one.
	if err := copyUntracked(project.Path, dir, target.CopyFiles); err != nil {
		return nil, err
	}

	root := dir
	if target.Cwd != "" {
		dir = filepath.Join(dir, target.Cwd)
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("this target runs in %s, which is not in that commit", target.Cwd)
		}
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}

	// A context of its own: the preview outlives the request that started it,
	// and outlives the swarm. It ends when Stop is called or the daemon does.
	runCtx, cancel := context.WithCancel(context.Background())

	// Through a shell, because a target is a command line as its author would
	// type it -- pipes, &&, and a compose file argument all included.
	cmd := exec.CommandContext(runCtx, "sh", "-c", target.Command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		// The contract: the daemon picks the port and the command binds it.
		// Every hosting platform does this, so most projects already read it.
		fmt.Sprintf("PORT=%d", port),
		fmt.Sprintf("ZERG_PREVIEW_PORT=%d", port),
		fmt.Sprintf("ZERG_COMMIT=%s", commit),
		// Where the operator's own checkout is, so a command can reach what
		// git does not carry without anything being copied:
		// `docker compose --env-file "$ZERG_PROJECT_DIR/.env" up`.
		fmt.Sprintf("ZERG_PROJECT_DIR=%s", project.Path),
		fmt.Sprintf("ZERG_PREVIEW_DIR=%s", root),
	)
	// Its own process group, so stopping it stops what it started. A compose
	// command that spawns docker, or a dev server that forks a watcher, leaves
	// children that outlive their parent otherwise -- and those children are
	// holding the port.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	tail := &ring{limit: logTail}
	// One pipe for both streams, interleaved the way a terminal shows them: a
	// build prints its progress on one and its error on the other, and reading
	// them apart puts the reason in a different place from what caused it.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting %q: %w", target.Name, err)
	}

	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			tail.add(sc.Text())
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	r := &run{cmd: cmd, cancel: cancel, log: tail, done: done,
		stopCmd: target.StopCommand, dir: dir}

	if err := waitForPort(runCtx, port, time.Duration(target.ReadySecs)*time.Second, done); err != nil {
		kill(cmd, done)
		cancel()
		// The log is the answer to "why", and it is the only place the answer
		// exists: a command that failed to bind said so in its output and
		// nowhere else.
		return nil, &StartError{Target: target.Name, Reason: err.Error(), Log: tail.string()}
	}

	a, err := m.db.AddArtifact(ctx, &store.Artifact{
		ProjectID: project.ID,
		TaskID:    nullable(taskID),
		Role:      "preview",
		Kind:      store.ArtifactService,
		Label:     target.Name,
		Port:      port,
		// The daemon's, not the swarm's: the reason to run this is to click it
		// after the pipeline finished, so the swarm stopping must leave it up.
		Owner: store.OwnerDaemon,
	})
	if err != nil {
		kill(cmd, done)
		cancel()
		return nil, err
	}
	r.artifact = a.ID

	m.mu.Lock()
	m.running[project.ID] = r
	m.mu.Unlock()

	// Nothing supervises this. A preview that died is a fact to report, not a
	// thing to restart: rerunning a build somebody is watching, without being
	// asked, is how two of them end up fighting for a port.
	go func() {
		<-done
		m.mu.Lock()
		if cur, ok := m.running[project.ID]; ok && cur == r {
			delete(m.running, project.ID)
		}
		m.mu.Unlock()
		if _, err := m.db.StopServices(context.Background(), project.ID, store.OwnerDaemon); err != nil {
			m.log.Warn("could not mark the preview stopped", "project", project.ID, "err", err)
		}
		m.log.Info("preview stopped", "project", project.ID, "target", target.Name)
	}()

	m.log.Info("preview up", "project", project.ID, "target", target.Name, "port", port,
		"commit", commit)
	return a, nil
}

// Stop ends a project's preview, and everything it started.
func (m *Manager) Stop(ctx context.Context, projectID string) {
	m.mu.Lock()
	r, ok := m.running[projectID]
	delete(m.running, projectID)
	m.mu.Unlock()
	if !ok {
		return
	}

	// Its own cleanup first, while what it started is still there to clean up.
	// `docker compose down` after the group has been killed still works, but
	// running it first is what lets compose stop containers rather than have
	// them killed underneath it.
	m.runStop(r)
	kill(r.cmd, r.done)
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		m.log.Warn("a preview did not stop in time", "project", projectID)
	}
	if _, err := m.db.StopServices(ctx, projectID, store.OwnerDaemon); err != nil {
		m.log.Warn("could not mark the preview stopped", "project", projectID, "err", err)
	}
}

// runStop runs a target's stop command, if it has one.
//
// Bounded and best effort: a cleanup that hangs must not hold up the stop, and
// one that fails has still left less behind than not running it. Its output
// goes to the log, which is where somebody looks when containers are still
// there afterwards.
func (m *Manager) runStop(r *run) {
	if strings.TrimSpace(r.stopCmd) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopGrace)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", r.stopCmd)
	cmd.Dir = r.dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		r.log.add(strings.TrimRight(string(out), "\n"))
	}
	if err != nil {
		m.log.Warn("a preview's stop command failed", "err", err, "command", r.stopCmd)
	}
}

// StopAll ends every preview, for a daemon shutting down. Nothing here
// survives the process that started it.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(ctx, id)
	}
}

// Log is what a running or just-failed preview has printed.
func (m *Manager) Log(projectID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.running[projectID]; ok {
		return r.log.string()
	}
	return ""
}

// StartError carries the output as well as the failure, because the output is
// where the reason is.
type StartError struct {
	Target string
	Reason string
	Log    string
}

func (e *StartError) Error() string {
	return fmt.Sprintf("%s did not come up: %s", e.Target, e.Reason)
}

// waitForPort blocks until something answers, the command exits, or time runs
// out.
func waitForPort(ctx context.Context, port int, limit time.Duration, exited <-chan struct{}) error {
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	deadline := time.After(limit)
	for {
		conn, err := net.DialTimeout("tcp", addr, dialEvery)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-exited:
			// It stopped on its own, which for a server means it failed to
			// start. Said as that rather than as a timeout, since waiting the
			// remaining two minutes would tell nobody anything.
			return fmt.Errorf("the command exited without serving on port %d", port)
		case <-deadline:
			return fmt.Errorf("nothing answered on port %d within %s", port, limit)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dialEvery):
		}
	}
}

// stopGrace is how long a command gets to shut down before it is killed.
// Long enough for `docker compose` to stop its containers, which is the slowest
// thing anybody puts in a target.
const stopGrace = 10 * time.Second

// kill ends the process group, not just the process, and does not stop when
// the leader exits.
//
// Two things make this less obvious than it looks. Killing the shell alone
// leaves whatever it started still holding the port -- `sh -c "npm run
// preview"` exits and vite does not. And a non-interactive shell sets SIGINT
// to ignore for anything it backgrounds, so an interrupt kills the shell and
// its child carries on: waiting for the leader and calling that success left a
// server running on the port a test then found still occupied.
//
// So: interrupt the group, wait for the leader as a courtesy to anything that
// cleans up on the way out, then kill the group regardless. A group with no
// members left answers ESRCH, which is the outcome that was wanted anyway.
func kill(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// No group to speak of: the process is all there is.
		_ = cmd.Process.Kill()
		return
	}

	_ = syscall.Kill(-pgid, syscall.SIGINT)
	select {
	case <-done:
	case <-time.After(stopGrace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// copyUntracked brings named files from the operator's checkout into the
// preview's.
//
// Named, never guessed: copying anything that looks like a secret into a
// directory the daemon runs commands in should be something somebody wrote
// down. Paths are resolved inside the project for the same reason
// `artifact add` is -- a target is configuration, and configuration that can
// name /etc/shadow is a way to read it.
func copyUntracked(projectDir, previewDir, list string) error {
	for _, raw := range strings.Split(list, "\n") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if filepath.IsAbs(name) || strings.Contains(name, "..") {
			return fmt.Errorf("%q: files to copy are paths inside the project", name)
		}
		src := filepath.Join(projectDir, name)
		info, err := os.Stat(src)
		if err != nil {
			// Said rather than skipped: the command is about to fail without
			// it, and this is the message that explains why.
			return fmt.Errorf("this target copies %s, which is not in %s", name, projectDir)
		}
		if info.IsDir() {
			return fmt.Errorf("%q is a directory; copy the files a command needs, not a tree", name)
		}
		body, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		dst := filepath.Join(previewDir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		// The permissions of the original, because a .env that arrives
		// world-readable is worse than one that does not arrive.
		if err := os.WriteFile(dst, body, info.Mode().Perm()); err != nil {
			return fmt.Errorf("copying %s into the preview: %w", name, err)
		}
	}
	return nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a port for the preview: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ring keeps the last bytes of output.
type ring struct {
	mu    sync.Mutex
	lines []string
	size  int
	limit int
}

func (r *ring) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	r.size += len(line) + 1
	for r.size > r.limit && len(r.lines) > 1 {
		r.size -= len(r.lines[0]) + 1
		r.lines = r.lines[1:]
	}
}

func (r *ring) string() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}
