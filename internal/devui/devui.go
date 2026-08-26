// Package devui runs the cockpit's dev server and puts it behind the daemon.
//
// The cockpit is generated rather than committed, so a fresh checkout has no
// UI until someone builds it. The honest instruction was "run ./build.sh", and
// it is a bad instruction to give someone who is about to change the UI: it
// costs eleven seconds a time and produces a bundle they will throw away on the
// next keystroke. The fast loop was always there, in Vite's own dev server, but
// it lived in CONTRIBUTING.md where nobody reads it at the moment they need it.
//
// So `zerg up` in a checkout starts Vite itself and proxies to it. One command,
// one origin, hot reload, and no build step to remember. It is deliberately
// invisible in a released binary: without a `web/` directory beside the daemon
// there is nothing to start, and the daemon says the cockpit is not built.
package devui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ErrNoSources means there is no cockpit source tree here to run, which is the
// ordinary case for an installed binary rather than a fault.
var ErrNoSources = errors.New("no web/ directory beside the daemon")

// Server is a running dev server and the proxy that fronts it.
type Server struct {
	Handler http.Handler
	URL     string

	cmd  *exec.Cmd
	log  *slog.Logger
	done chan struct{}
}

// Find returns the cockpit's source directory, or ErrNoSources.
//
// Checked relative to the working directory rather than to the executable: a
// developer runs `./zerg up` from the repository root, and `go run ./cmd/zerg`
// puts the binary in a temporary directory that has no relation to the sources.
func Find() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Walk up a couple of levels so this works from cmd/zerg as well as the
	// root, and stop there: further up is somebody else's repository.
	for i := 0; i < 3; i++ {
		web := filepath.Join(dir, "web")
		if _, err := os.Stat(filepath.Join(web, "package.json")); err == nil {
			return web, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", ErrNoSources
}

// Start runs the dev server and waits for it to answer.
//
// Blocking until it is ready, rather than proxying to a port that is not
// listening yet: the daemon prints its own URL when this returns, and a URL
// that 502s for the first three seconds is a worse first impression than three
// seconds of silence with a line saying what is happening.
func Start(ctx context.Context, log *slog.Logger, webDir string) (*Server, error) {
	for _, tool := range []string{"node", "pnpm"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s is not on PATH, so the cockpit cannot be built or served", tool)
		}
	}

	// pnpm install is the slow, once-per-clone part, and skipping it silently
	// would fail later with a missing-module error from Vite that says nothing
	// about what to do.
	if _, err := os.Stat(filepath.Join(webDir, "node_modules")); errors.Is(err, os.ErrNotExist) {
		log.Info("installing the cockpit's dependencies, once", "dir", webDir)
		install := exec.CommandContext(ctx, "pnpm", "install", "--frozen-lockfile")
		install.Dir = webDir
		install.Stdout, install.Stderr = os.Stderr, os.Stderr
		if err := install.Run(); err != nil {
			return nil, fmt.Errorf("installing the cockpit's dependencies: %w", err)
		}
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}

	// --host 127.0.0.1 is not decoration. Vite's default is "localhost", which
	// on macOS resolves to ::1 first, so it binds IPv6 only and a probe of
	// 127.0.0.1 is refused: the daemon waited the full ninety seconds for a
	// server that had been ready in one, then gave up. Naming the address makes
	// the probe, the proxy and the server agree.
	//
	// --strictPort so a port taken between the probe and the launch is an
	// error rather than Vite quietly choosing another one and this proxy
	// pointing at nothing.
	cmd := exec.Command("pnpm", "exec", "vite",
		"--host", "127.0.0.1", "--port", fmt.Sprint(port), "--strictPort")
	cmd.Dir = webDir
	superviseProcess(cmd, 5*time.Second)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the cockpit's dev server: %w", err)
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", fmt.Sprint(port))}
	s := &Server{
		cmd:  cmd,
		log:  log,
		URL:  target.String(),
		done: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		close(s.done)
		// Expected during shutdown, when the daemon killed it on purpose.
		if err != nil && ctx.Err() == nil {
			log.Warn("the cockpit's dev server exited", "err", err)
		}
	}()

	if err := wait(ctx, target.String(), s.done, 90*time.Second); err != nil {
		s.Stop()
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// A dev server that has died mid-session must say so rather than
		// answering with an empty 502 that reads as a daemon fault.
		http.Error(w, "the cockpit's dev server is not responding: "+err.Error(), http.StatusBadGateway)
	}
	s.Handler = proxy
	return s, nil
}

// Stop ends the dev server and waits for it to go.
func (s *Server) Stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	killGroup(s.cmd)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.log.Warn("the cockpit's dev server did not exit")
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a port for the dev server: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// wait polls until the dev server answers, the process dies, or time runs out.
//
// Polling rather than reading Vite's "ready in" line: that line is a log format
// belonging to another project, and a version that reworded it would leave the
// daemon waiting forever on a server that was already up.
func wait(ctx context.Context, target string, done <-chan struct{}, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		select {
		case <-done:
			return errors.New("the cockpit's dev server exited before it was ready")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the cockpit's dev server did not answer within %s", limit)
		}
		time.Sleep(150 * time.Millisecond)
	}
}
