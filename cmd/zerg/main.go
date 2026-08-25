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
	"syscall"
	"time"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/adapter/claudeharness"
	"github.com/konfessor/zerg/internal/adapter/piharness"
	"github.com/konfessor/zerg/internal/agent"
	"github.com/konfessor/zerg/internal/api"
	"github.com/konfessor/zerg/internal/event"
	"github.com/konfessor/zerg/internal/nydus"
	"github.com/konfessor/zerg/internal/overmind"
	"github.com/konfessor/zerg/internal/preflight"
	"github.com/konfessor/zerg/internal/store"
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
	case "ask":
		return runAsk(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `zerg — multi-agent coding orchestrator

  zerg up [--addr host:port] [--db path] [--verbose]
      Run the overmind and serve the cockpit.

Run by agents, not by you:

  zerg next [--wait 30s]              claim work
  zerg done --lease <id>              acknowledge it
  zerg send --to <role> --commit HEAD --task <id>
  zerg ask "<question>"               ask the operator

Everything is configured in the cockpit; there are no config files.
`)
}

func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "address to serve the cockpit on; port 0 picks a free one")
	dbPath := fs.String("db", "", "database path (default ~/.zerg/zerg.db)")
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

	nyd := nydus.New(db, nydus.WithIntegrator(nydus.Git{}))
	bus := event.NewBus()

	// Something has to watch the agents, or the orchestrator reproduces the
	// failure it was built to prevent.
	event.LogEvents(ctx, bus, log)

	// And something has to keep it. Both events and usage are reported once, as
	// they happen, and cannot be recovered later — an unrecorded turn is
	// unrecorded permanently.
	event.Record(ctx, bus, db, log)

	// Events are the expensive tier and exist to replay recent work, so they
	// age out. Costs and outcomes live elsewhere and do not.
	api.PruneEvents(ctx, db, log, eventRetention, retentionSweep)

	// Agents are children of this process, so any lease still open belongs to a
	// process that no longer exists. Requeue it now rather than letting the
	// work sit untouched until its deadline.
	if n, err := nyd.ReclaimOrphanedLeases(ctx); err != nil {
		log.Error("could not reclaim leases from the previous run", "err", err)
	} else if n > 0 {
		log.Info("reclaimed work from the previous run", "leases", n)
	}

	agents := agent.NewServer(db, nyd, log)
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

	srv := &http.Server{
		Handler: api.New(api.Deps{
			DB: db, Log: log, Registry: registry,
			Overmind: over, Nydus: nyd, Bus: bus,
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Listen before announcing, so the printed URL is always a port that is
	// actually accepting connections.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}

	log.Info("overmind up", "url", "http://"+ln.Addr().String(), "db", *dbPath, "socket", socket)
	fmt.Printf("Cockpit: http://%s\n", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// defaultHarness is what a freshly seeded library points its roles at.
// Whether that harness is actually usable is preflight's question, not this
// one: the answer belongs in a readiness panel with a remedy attached, not in
// a silent decision here.
func defaultHarness() string { return claudeharness.New().Name() }
