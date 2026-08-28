package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kconfesor/zerg/internal/agent"
)

// The agent-facing side of the binary. An agent gets these five verbs and
// nothing else — no scripts synced onto its PATH, no directory to infer its
// identity from, no tmux to address.

func runNext(args []string) error {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for work before giving up")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}

	// The context outlives the wait so a slow reply is not cut off mid-answer.
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()

	work, err := client.Next(ctx, *wait)
	if errors.Is(err, agent.ErrNoWork) {
		// Nothing queued is an ordinary outcome. Exit 0 and print nothing, so
		// an agent reading this does not mistake quiet for failure.
		return nil
	}
	if err != nil {
		return err
	}
	return printJSON(work)
}

func runDone(args []string) error {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	lease := fs.String("lease", "", "the lease being acknowledged")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lease == "" {
		return errors.New("done needs --lease, the id from the work you claimed")
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Done(ctx, *lease)
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "recipient role; omit to finish the task (terminal role only)")
	task := fs.String("task", "", "the task id this work belongs to")
	commit := fs.String("commit", "", "the commit this handoff points at")
	body := fs.String("body", "", "a short note for the recipient")
	kind := fs.String("kind", "handoff", "handoff or note")
	priority := fs.Int("priority", 50, "lower is sooner")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := client.Send(ctx, agent.SendArgs{
		To: *to, TaskID: *task, Kind: *kind,
		Commit: *commit, Body: *body, Priority: *priority,
	})
	if err != nil {
		return err
	}
	return printJSON(out)
}

func runAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	task := fs.String("task", "", "the task the question is about")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for an answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New(`ask needs a question, e.g. zerg ask "should this be idempotent?"`)
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()

	answer, err := client.Ask(ctx, fs.Arg(0), *task, *wait)
	if err != nil {
		return err
	}
	return printJSON(answer)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// runArtifact records something the agent produced.
//
// Two subcommands under one verb because they are one act with two shapes:
//
//	zerg artifact add ./coverage.html --label "Coverage report"
//	zerg artifact serve --port 5173   --label "Dev server"
//
// A file is copied into the daemon's store, so the agent's worktree can be
// pruned and the file survives; a service is a port, which is only true while
// the process holding it is alive.
func runArtifact(args []string) error {
	if len(args) == 0 {
		return errors.New("artifact needs a subcommand: add <path>, or serve --port <n>")
	}

	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("artifact "+sub, flag.ContinueOnError)
	label := fs.String("label", "", "what to call it in the cockpit")
	task := fs.String("task", "", "the task it belongs to (default: the one this role is holding)")
	port := fs.Int("port", 0, "the port a service is listening on")

	req := agent.ArtifactArgs{}
	switch sub {
	case "add":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return errors.New("artifact add needs a path: zerg artifact add ./report.html")
		}
		req.Kind = "file"
		req.Path = fs.Arg(0)
	case "serve":
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *port == 0 {
			return errors.New("artifact serve needs --port, the port your service is listening on")
		}
		req.Kind = "service"
		req.Port = *port
	default:
		return fmt.Errorf("unknown artifact subcommand %q; it is add or serve", sub)
	}
	req.Label, req.TaskID = *label, *task

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	made, err := client.Artifact(ctx, req)
	if err != nil {
		return err
	}
	return printJSON(made)
}

// runRemember writes down what this agent worked out about serving the project.
//
//	zerg remember "serves with: docker compose -f infra/dev/compose.yml up.
//	               needs .env, which is not in the repository."
//
// Read back to the next runner before it starts, which is the whole reason a
// second preview is faster than the first.
func runRemember(args []string) error {
	fs := flag.NewFlagSet("remember", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	note := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if note == "" {
		return errors.New(`remember needs the note: zerg remember "how this project serves itself"`)
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Remember(ctx, note)
}
