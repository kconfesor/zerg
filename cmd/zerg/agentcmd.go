package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/konfessor/zerg/internal/agent"
)

// The agent-facing side of the binary. An agent gets these four verbs and
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
