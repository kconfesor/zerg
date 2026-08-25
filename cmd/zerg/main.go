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
	"syscall"
	"time"

	"github.com/konfessor/zerg/internal/adapter"
	"github.com/konfessor/zerg/internal/adapter/claudeharness"
	"github.com/konfessor/zerg/internal/api"
	"github.com/konfessor/zerg/internal/store"
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

	srv := &http.Server{
		Handler:           api.New(db, log, registry).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Listen before announcing, so the printed URL is always a port that is
	// actually accepting connections.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}

	log.Info("overmind up", "url", "http://"+ln.Addr().String(), "db", *dbPath)
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
