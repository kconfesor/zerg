// Command zerg is the orchestrator daemon and the agent-facing client.
//
// One binary, two audiences: `zerg up` runs the overmind and serves the
// cockpit; `zerg next|done|send|ask` is what an agent subprocess calls. The
// agent side arrives with the nydus transport in milestone 2 — see
// ARCHITECTURE.md §7.2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
	"github.com/kconfesor/zerg/internal/adapter/claudeharness"
	"github.com/kconfesor/zerg/internal/adapter/piharness"
	"github.com/kconfesor/zerg/internal/agent"
	"github.com/kconfesor/zerg/internal/api"
	"github.com/kconfesor/zerg/internal/artifact"
	"github.com/kconfesor/zerg/internal/chat"
	"github.com/kconfesor/zerg/internal/devui"
	"github.com/kconfesor/zerg/internal/event"
	"github.com/kconfesor/zerg/internal/nydus"
	"github.com/kconfesor/zerg/internal/overmind"
	"github.com/kconfesor/zerg/internal/preflight"
	"github.com/kconfesor/zerg/internal/preview"
	"github.com/kconfesor/zerg/internal/store"
	"github.com/kconfesor/zerg/internal/tailnet"
)

// How long an agent transcript stays replayable, and how often the sweep runs.
//
// Events are the expensive tier — roughly 40 MB a day at five active roles —
// and exist to replay recent work. A task's cost, duration and outcome live in
// usage_turns and tasks, and are kept indefinitely, so ageing a transcript out
// loses the narrative and none of the metrics.
const (
	eventRetention = 14 * 24 * time.Hour
	retentionSweep = 6 * time.Hour
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "zerg: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "up":
		return runUp(args[1:])

	// The agent-facing verbs. These run inside a spawned agent and reach the
	// overmind over the unix socket named in its environment.
	case "next":
		return runNext(args[1:])
	case "done":
		return runDone(args[1:])
	case "send":
		return runSend(args[1:])
	case "artifact":
		return runArtifact(args[1:])
	case "ask":
		return runAsk(args[1:])
	case "version", "--version", "-v":
		printVersion()
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// printVersion reports what this binary was built from.
//
// Read out of the build info rather than stamped by ldflags, so a plain
// `go build` and `go install` say something true instead of "dev". It is the
// first thing worth knowing about a bug report, which is why the issue template
// asks for it.
func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("zerg (unknown build)")
		return
	}
	var revision, modified, when string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			when = s.Value
		}
	}
	// A pseudo-version already contains the revision, and printing both says
	// the same hash twice.
	version := info.Main.Version
	if version == "" || version == "(devel)" || strings.HasPrefix(version, "v0.0.0-") {
		version = "devel"
	}
	out := "zerg " + version
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		out += " (" + revision
		if modified == "true" {
			// Says so plainly: a report against a dirty tree is a report about
			// code nobody else has.
			out += ", dirty"
		}
		if when != "" {
			out += ", " + when
		}
		out += ")"
	}
	fmt.Println(out + ", " + info.GoVersion + " " + runtime.GOOS + "/" + runtime.GOARCH)
}

func usage() {
	fmt.Fprint(os.Stderr, `zerg, a multi-agent coding orchestrator

  zerg up [--addr host:port] [--db path] [--no-dev-ui] [--verbose]
      Run the overmind and serve the cockpit. In a checkout with no cockpit
      compiled in, this also runs the cockpit's dev server and serves it here,
      hot-reloading; --no-dev-ui turns that off.

Run by agents, not by you:

  zerg next [--wait 30s]              claim work
  zerg done --lease <id>              acknowledge it
  zerg send --to <role> --commit HEAD --task <id>
  zerg ask "<question>"               ask the operator
  zerg artifact add <path>            keep a file for a person to look at
  zerg artifact serve --port <n>      register a service you started

  zerg version                        what this binary was built from

Everything is configured in the cockpit; there are no config files.
`)
}

// tailnetHostFor is the MagicDNS name this daemon serves, resolved once.
//
// cfg.TailnetHost is usually empty: the name is discovered rather than
// configured. Three things need it and each used to find out for itself, which
// is how the guard came to refuse the URL the banner had just printed, and how
// a certificate could be issued for a name neither of the other two had.
//
// Resolved when there is a reason to: tailscale TLS needs a name to get a
// certificate for, and any non-loopback bind can be reached by that name even
// with TLS off, which `zerg up --no-tls --addr $(tailscale ip -4):7717` is.
//
// The status comes back too, so the caller that cannot continue without a name
// can say why rather than reporting an empty one. The probe is a parameter
// because it shells out to tailscale, and this decision is worth testing
// without one.
func tailnetHostFor(cfg store.Config, probe func() tailnet.Status) (string, tailnet.Status) {
	if cfg.TailnetHost != "" {
		return cfg.TailnetHost, tailnet.Status{Available: true, DNSName: cfg.TailnetHost}
	}
	if cfg.TLSMode != store.TLSTailscale && cfg.LoopbackOnly() {
		return "", tailnet.Status{}
	}
	st := probe()
	if !st.Available {
		return "", st
	}
	return st.DNSName, st
}

func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	addr := fs.String("addr", "", "override the stored bind address for this run only")
	noTLS := fs.Bool("no-tls", false, "serve plain HTTP for this run, ignoring the stored TLS setting")
	dbPath := fs.String("db", "", "database path (default ~/.zerg/zerg.db)")
	// An escape hatch for the case where the sources are present and you want
	// the daemon alone: another Vite already running, or a machine without node.
	noDev := fs.Bool("no-dev-ui", false, "do not start the cockpit's dev server even if its sources are here")
	verbose := fs.Bool("verbose", false, "log every request")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *dbPath == "" {
		p, err := store.DefaultPath()
		if err != nil {
			return err
		}
		*dbPath = p
	}

	// Signals cancel the context, which shuts the server down gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Seeding is idempotent and never clobbers an edited role, so it is safe
	// on every start. Configuration lives in the database precisely so that a
	// restart does not overwrite what the user changed.
	if err := store.Seed(ctx, db, defaultHarness()); err != nil {
		return fmt.Errorf("seeding the role library: %w", err)
	}

	registry := adapter.NewRegistry()
	registry.Register(claudeharness.New())
	registry.Register(piharness.New())

	// State lives beside the database, so a project directory holds nothing but
	// git artifacts.
	stateDir := filepath.Join(filepath.Dir(*dbPath), "state")

	nyd := nydus.New(db, nydus.WithIntegrator(nydus.Git{}),
		// Reclaiming disk when a card lands is the policy that keeps a long
		// run bounded: build output is per role, and nothing needs it once the
		// work is merged.
		nydus.WithOnTaskDone(func(ctx context.Context, projectID, _ string) {
			sweepOnDone(ctx, db, projectID, log)
		}))
	bus := event.NewBus()

	// Something has to watch the agents, or the orchestrator reproduces the
	// failure it was built to prevent.
	event.LogEvents(ctx, bus, log)

	// And something has to keep it. Both events and usage are reported once, as
	// they happen, and cannot be recovered later — an unrecorded turn is
	// unrecorded permanently.
	recorder := event.Record(ctx, bus, db, log)

	// Settings live in the database like everything else, so the cockpit can
	// change them. The flag stays as an override for one run, which is what you
	// want when a stored address turns out not to bind.
	cfg, err := db.GetConfig(ctx)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	// The way out of a TLS setting that cannot be satisfied. Without it, saving
	// one locks you out of the settings view that sets it: the daemon refuses
	// to start, and the only remaining repair is editing the database by hand.
	if *noTLS {
		cfg.TLSMode = store.TLSOff
	}

	// Events are the expensive tier and exist to replay recent work, so they
	// age out. Costs and outcomes live elsewhere and do not. The window is read
	// on each sweep, so changing it in Settings takes effect without a restart —
	// which is what Settings has always said it does.
	// Beside the database rather than inside it: a screenshot in a SQLite row
	// is read into memory to be served and competes for the write lock the
	// whole daemon shares (internal/artifact).
	blobs := artifact.New(filepath.Join(filepath.Dir(*dbPath), "artifacts"))
	api.PruneEvents(ctx, db, blobs, log, retentionSweep)

	// Agents are children of this process, so any lease still open belongs to a
	// process that no longer exists. Requeue it now rather than letting the
	// work sit untouched until its deadline.
	if n, err := nyd.ReclaimOrphanedLeases(ctx); err != nil {
		log.Error("could not reclaim leases from the previous run", "err", err)
	} else if n > 0 {
		log.Info("reclaimed work from the previous run", "leases", n)
	}

	// And any approval the previous run was carrying out when it died. The
	// decision claims the approval, releases the write lock, runs git, then
	// records the outcome; killed in between, it stays "integrating" — hidden
	// from Attention, so nobody is asked about it, over work that may already
	// have landed on the base branch.
	if settled, released, err := nyd.ReconcileIntegrating(ctx); err != nil {
		log.Error("could not settle interrupted approvals from the previous run", "err", err)
	} else if settled > 0 || released > 0 {
		log.Info("settled interrupted approvals from the previous run",
			"completed", settled, "returned_to_pending", released)
	}

	// Chat runs outside the pipeline: its own process, its own worktree, no
	// capability token, so a question cannot disturb work in flight.
	chatMgr := chat.NewManager(db, registry, bus, log, stateDir)
	defer chatMgr.StopAll()

	agents := agent.NewServer(db, nyd, blobs, log)
	socket := filepath.Join(stateDir, "agent.sock")
	if err := agents.Listen(socket); err != nil {
		return err
	}
	defer agents.Close()

	over := overmind.New(overmind.Config{
		DB: db, Nydus: nyd, Registry: registry,
		Preflight: preflight.NewRunner(db, registry),
		Bus:       bus, Agents: agents, Log: log, StateDir: stateDir,
	})
	// Agents are children of this process, so leaving them running after the
	// daemon exits would orphan work nobody is supervising.
	defer over.StopAll(context.Background(), "the daemon shut down")

	// Whatever the last run left behind, before anything new is written.
	sweepOnStart(ctx, db, cfg, log)

	// A checkout gets a working, hot-reloading cockpit from `zerg up` alone.
	//
	// The cockpit is generated rather than committed, so a fresh clone has no
	// UI compiled in. Telling someone to run ./build.sh is the wrong advice for
	// the person most likely to hit this, who is about to change the UI and
	// would pay eleven seconds per keystroke for a bundle they throw away. So
	// if the sources are here and nothing was embedded, the daemon runs Vite
	// itself and proxies to it: one command, one origin, hot reload.
	//
	// A released binary has no web/ beside it, finds nothing, and serves what
	// was compiled in. Nothing here runs in that case.
	var ui http.Handler
	if !api.Embedded() && !*noDev {
		if webDir, err := devui.Find(); err == nil {
			log.Info("no cockpit was compiled in, and its sources are here; starting the dev server", "dir", webDir)
			dev, err := devui.Start(ctx, log, webDir)
			if err != nil {
				log.Error("the cockpit's dev server could not start; serving the built-in page instead", "err", err)
			} else {
				defer dev.Stop()
				ui = dev.Handler
				log.Info("the cockpit is hot-reloading from source", "vite", dev.URL)
			}
		}
	}

	// One probe for the certificate, the guard and the banner.
	tailnetHost, tailnetStatus := tailnetHostFor(cfg, func() tailnet.Status { return tailnet.Probe(ctx) })

	// The services an agent started, proxied on an origin of their own.
	//
	// A separate listener rather than a route on the cockpit, which is §13.4:
	// a dev server an agent wrote is code running in a browser, and on the
	// cockpit's origin it would have same-origin access to a command API with
	// no authentication. A different port is a different origin, and that is
	// the entire mechanism.
	//
	// The port is chosen by the operating system and not configured. It is an
	// implementation detail of a link the daemon builds itself, and asking
	// somebody to pick a second port -- and to keep it free -- buys nothing.
	// A failure here costs the service viewer and not the daemon.
	proxyPort, serveProxy, closeProxy := listenProxy(db, log)
	defer closeProxy()

	// Running the project's own code, at a commit somebody chose. Its
	// processes are this daemon's children and do not survive it, which is
	// what the deferred stop is for: a compose stack left running after the
	// daemon exits is a port nobody owns and nothing will clean up.
	previews := preview.NewManager(db, log, stateDir)
	defer previews.StopAll(context.Background())

	srv := &http.Server{
		Handler: api.New(api.Deps{
			DB: db, Log: log, Registry: registry,
			Overmind: over, Nydus: nyd, Bus: bus, Recorder: recorder, Applied: cfg.Listener(), Chat: chatMgr,
			TailnetHost: tailnetHost, UI: ui, Blobs: blobs, ProxyPort: proxyPort,
			Preview: previews,
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// A body that arrives a byte at a time holds a connection open
		// indefinitely; an idle keep-alive holds one for nothing. Generous
		// enough for a slow tailnet link and finite, which is the point.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
		// Deliberately no WriteTimeout: the activity stream is a long-lived
		// WebSocket and a write deadline on the server would cut it. The
		// stream sets its own per-write deadline instead.
	}

	tlsCert, tlsKey, err := resolveTLS(ctx, cfg, tailnetHost, tailnetStatus, filepath.Join(stateDir, "certs"))
	if err != nil {
		// Loud, and with the way out. Starting on plain HTTP instead would
		// serve an address the operator asked to be encrypted, which is the
		// one outcome worse than not starting.
		return fmt.Errorf("%w\n\nTo start anyway and change it in Settings, run: zerg up --no-tls", err)
	}

	// Listen before announcing, so the printed URL is always a port that is
	// actually accepting connections.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	}

	scheme := "http"
	if tlsCert != "" {
		scheme = "https"
	}
	// The MagicDNS name, not the IP: a certificate is issued for the name, so
	// the address that avoids a warning is the one worth printing.
	shown := ln.Addr().String()
	if cfg.TLSMode == store.TLSTailscale && tailnetHost != "" {
		_, port, _ := net.SplitHostPort(cfg.Addr)
		shown = net.JoinHostPort(tailnetHost, port)
	}

	// A second listener on loopback, plain, when the first is somewhere else.
	// Local work should not have to type a MagicDNS name, and a settings change
	// that breaks the main listener must never make settings unreachable.
	var localLn net.Listener
	if cfg.LocalAccess && !cfg.LoopbackOnly() {
		_, port, _ := net.SplitHostPort(cfg.Addr)
		localAddr := net.JoinHostPort("127.0.0.1", port)
		localLn, err = net.Listen("tcp", localAddr)
		if err != nil {
			// Not fatal. Something else on the port is a reason to lose the
			// convenience, not the daemon.
			log.Warn("local access unavailable", "addr", localAddr, "err", err)
		}
	}

	// Same scheme as the cockpit, necessarily: an https page cannot embed an
	// http iframe, so a plain proxy beside a TLS cockpit would be a viewer
	// that never loads and a browser console nobody reads.
	serveProxy(tlsCert, tlsKey)

	log.Info("overmind up", "url", scheme+"://"+shown, "db", *dbPath, "socket", socket)
	fmt.Printf("Cockpit: %s://%s\n", scheme, shown)
	if localLn != nil {
		fmt.Printf("Locally: http://%s\n", localLn.Addr().String())
	}
	warnIfReachable(cfg.Addr, tlsCert != "")

	errCh := make(chan error, 1)
	if localLn != nil {
		// Same handler, same state — a different door into one daemon, never a
		// second copy of anything.
		go func() {
			if err := srv.Serve(localLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("local listener stopped", "err", err)
			}
		}()
	}
	go func() {
		var err error
		if tlsCert != "" {
			err = srv.ServeTLS(ln, tlsCert, tlsKey)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		// Every service belonged to a process this daemon owned, so none of
		// them survives it.
		if n, err := db.StopServices(context.Background(), "", ""); err != nil {
			log.Warn("could not mark services stopped", "err", err)
		} else if n > 0 {
			log.Info("services stopped with the daemon", "services", n)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// resolveTLS turns the configured mode into a certificate and key, or empty
// strings for plain HTTP.
//
// A tailscale certificate is requested on every start: the command is
// idempotent, reusing a valid certificate and renewing one near expiry, so
// there is nothing to cache and nothing to go stale.
func resolveTLS(ctx context.Context, cfg store.Config, tailnetHost string, st tailnet.Status, certDir string) (certFile, keyFile string, err error) {
	switch cfg.TLSMode {
	case store.TLSOff:
		return "", "", nil

	case store.TLSFiles:
		return cfg.CertFile, cfg.KeyFile, nil

	case store.TLSTailscale:
		// The name resolved once at startup, rather than probed again here. A
		// second probe can answer differently from the first, and a certificate
		// issued for a name the guard refuses and the banner never printed is
		// the same failure this was meant to end.
		if tailnetHost == "" {
			return "", "", fmt.Errorf("TLS is set to tailscale but %s", st.Reason)
		}
		return tailnet.EnsureCert(ctx, tailnetHost, certDir)
	}
	return "", "", fmt.Errorf("unknown TLS mode %q", cfg.TLSMode)
}

// sweepOnStart reclaims whatever the previous run left in the worktrees.
//
// Best effort and never fatal: failing to free disk is not a reason to refuse
// to start, and the operator would rather have the daemon than the bytes.
func sweepOnStart(ctx context.Context, db *store.DB, cfg store.Config, log *slog.Logger) {
	if cfg.CleanPolicy != store.CleanOnStart {
		return
	}
	projects, err := db.ListProjects(ctx)
	if err != nil {
		log.Error("startup sweep: could not list projects", "err", err)
		return
	}
	for _, p := range projects {
		freed, pruned, err := api.Sweep(ctx, db, p.ID, cfg)
		if err != nil {
			log.Error("startup sweep", "project", p.Name, "err", err)
			continue
		}
		if freed > 0 || len(pruned) > 0 {
			log.Info("startup sweep", "project", p.Name,
				"freed_mb", freed/(1024*1024), "branches_pruned", len(pruned))
		}
	}
}

// sweepOnDone reclaims a project's disk when a card finishes, if that is the
// configured policy.
//
// Best effort by design: the task is complete either way, and failing to free
// bytes must never look like failing to finish work.
func sweepOnDone(ctx context.Context, db *store.DB, projectID string, log *slog.Logger) {
	cfg, err := db.GetConfig(ctx)
	if err != nil || cfg.CleanPolicy != store.CleanOnDone {
		return
	}
	freed, pruned, err := api.Sweep(ctx, db, projectID, cfg)
	if err != nil {
		log.Error("sweep after task", "project", projectID, "err", err)
		return
	}
	if freed > 0 || len(pruned) > 0 {
		log.Info("swept after task", "project", projectID,
			"freed_mb", freed/(1024*1024), "branches_pruned", len(pruned))
	}
}

// warnIfReachable says so, once, when the cockpit is listening somewhere other
// than loopback.
//
// There is no authentication on the cockpit. Anything that can reach the port
// can start agents, read every transcript, and see the paths of the
// repositories being worked on. On loopback that is the same trust boundary as
// the shell it was launched from; on any other interface it is not, and the
// difference is worth one line at startup rather than a discovery later.
//
// Binding to a private-network interface such as Tailscale is a reasonable
// thing to want — a phone on the tailnet is still your own device. Binding
// 0.0.0.0 also exposes it to whatever else shares the local network.
func warnIfReachable(addr string, encrypted bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	switch host {
	case "", "127.0.0.1", "::1", "localhost":
		return
	}
	plain := ""
	if !encrypted {
		plain = " and no TLS"
	}
	if host == "0.0.0.0" || host == "::" {
		fmt.Fprintln(os.Stderr,
			"warning: listening on every interface, with no authentication"+plain+".\n"+
				"         Anything that can reach this port can start agents and read every\n"+
				"         transcript. Prefer binding one interface, e.g. --addr <tailscale-ip>:7717")
		return
	}
	fmt.Fprintf(os.Stderr,
		"note: reachable at %s beyond this machine, with no authentication%s.\n"+
			"      Treat anything that can route to it as trusted.\n", addr, plain)
}

// defaultHarness is what a freshly seeded library points its roles at.
// Whether that harness is actually usable is preflight's question, not this
// one: the answer belongs in a readiness panel with a remedy attached, not in
// a silent decision here.
func defaultHarness() string { return claudeharness.New().Name() }
