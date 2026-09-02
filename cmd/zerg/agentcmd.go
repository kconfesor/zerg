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
	var options optionList
	fs.Var(&options, "option", "an answer to offer; repeat it, once per option")
	words, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	// Joined rather than taking the first word, because an unquoted question is
	// the shell slip an agent actually makes, and half a question filed against
	// nothing is one nobody can answer.
	question := strings.TrimSpace(strings.Join(words, " "))
	if question == "" {
		return errors.New(`ask needs a question, e.g. zerg ask "should this be idempotent?"`)
	}

	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()

	answer, err := client.Ask(ctx, question, *task, options, *wait)
	if err != nil {
		return err
	}
	return printJSON(answer)
}

func runApprove(args []string) error {
	return runDecide(args, "approve", true)
}

func runReject(args []string) error {
	return runDecide(args, "reject", false)
}

func runDecide(args []string, verb string, ok bool) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	id := fs.String("id", "", "the approval id from the decide envelope")
	note := fs.String("note", "", "the rationale, required")
	commit := fs.String("commit", "", "the commit that recorded the decision")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("%s needs --id, from the decide envelope", verb)
	}
	if strings.TrimSpace(*note) == "" {
		return fmt.Errorf("%s needs --note: the rationale", verb)
	}
	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if ok {
		return client.Approve(ctx, *id, *note, *commit)
	}
	return client.Reject(ctx, *id, *note, *commit)
}

func runAnswer(args []string) error {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	id := fs.String("id", "", "the clarification id from the decide envelope")
	commit := fs.String("commit", "", "the commit that recorded the decision")
	words, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	answer := strings.TrimSpace(strings.Join(words, " "))
	if *id == "" {
		return errors.New("answer needs --id, from the decide envelope")
	}
	if answer == "" {
		return errors.New("answer needs the text of the answer")
	}
	client, err := agent.NewClientFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Answer(ctx, *id, answer, *commit)
}

// parseAnywhere parses flags wherever they appear, and returns the words that
// are not flags.
//
// Go's flag package stops at the first word that is not a flag, and every form
// this binary documents puts the subject first: `zerg ask "<question>" --task
// "<name>" --option ...`, `zerg artifact add ./report.html --label "<what>"`.
// Parsed straight through, everything after that first word is positional and
// silently ignored -- a question filed against no card, offering none of the
// options it just enumerated, and an artifact with no label. Re-parsing what is
// left after each positional is the ordinary permute, and a flag keeps its
// value (`--task Login`) because Parse consumes both itself.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return words, nil
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// optionList collects a repeated --option. A repeated flag rather than one
// comma-separated string: an option is a sentence a person reads off a screen,
// and it can legitimately contain a comma.
type optionList []string

func (o *optionList) String() string { return strings.Join(*o, ", ") }

func (o *optionList) Set(v string) error {
	*o = append(*o, v)
	return nil
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
		paths, err := parseAnywhere(fs, rest)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return errors.New("artifact add needs a path: zerg artifact add ./report.html")
		}
		if len(paths) > 1 {
			return fmt.Errorf("artifact add takes one path, and was given %d (%s); quote a path with spaces in it",
				len(paths), strings.Join(paths, ", "))
		}
		req.Kind = "file"
		req.Path = paths[0]
	case "serve":
		if _, err := parseAnywhere(fs, rest); err != nil {
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
