package main

import (
	"context"
	"log/slog"

	"github.com/kconfesor/zerg/internal/store"
)

// autoRun deploys a task that just landed, when the card asked for that.
//
// The decision is the card's, not the project's. It was a project-wide switch
// first, which is either off -- and nobody ever sees a preview -- or on, and
// the project pays an agent turn to preview a README fix. Most cards are not
// worth looking at; the person writing one knows which theirs is.
//
// Nothing here can fail the completion. The work landed; what failed is a
// convenience, and a card marked done that then reports an error about a
// preview would be describing the wrong thing.
func autoRun(ctx context.Context, db *store.DB, projectID, taskID, commit string, log *slog.Logger) {
	if !deploysOnLanding(ctx, db, taskID, commit) || runners == nil {
		return
	}
	// Its own context: the completion that triggered this is finished, and its
	// cancellation would take the preview with it.
	go func() {
		if err := runners.Run(context.Background(), projectID, commit, taskID); err != nil {
			// Logged rather than raised. If the runner needs something it will
			// ask, and that question reaches Attention like any other; this is
			// for the case where it could not even start.
			log.Warn("could not deploy a finished task",
				"project", projectID, "task", taskID, "err", err)
		}
	}()
}

// deploysOnLanding is whether this card asked to be deployed when it landed.
//
// Separated from the doing so it can be asserted against a database rather
// than by watching for an agent to start.
func deploysOnLanding(ctx context.Context, db *store.DB, taskID, commit string) bool {
	// Nothing to deploy. A card can finish without leaving a commit -- rejected,
	// or work that changed nothing -- and there is no state to put anywhere.
	if commit == "" {
		return false
	}
	task, err := db.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	return task.Deploy == store.DeployLocal
}
