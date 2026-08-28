package main

import (
	"context"
	"log/slog"

	"github.com/kconfesor/zerg/internal/store"
)

// autoRun starts a preview of a task that just landed, when the project asked
// for that.
//
// Off by default and per project, because every run is an agent turn and
// therefore money. On, it answers "what does it look like" before anybody
// thinks to ask -- which is the whole reason somebody sets it.
//
// Nothing here can fail the completion. The work landed; what failed is a
// convenience, and a card marked done that then reports an error about a
// preview would be describing the wrong thing.
func autoRun(ctx context.Context, db *store.DB, projectID, taskID, commit string, log *slog.Logger) {
	if commit == "" {
		return
	}
	project, err := db.GetProject(ctx, projectID)
	if err != nil || !project.AutoRun {
		return
	}
	if runners == nil {
		return
	}
	// Its own context: the completion that triggered this is finished, and its
	// cancellation would take the preview with it.
	go func() {
		if err := runners.Run(context.Background(), projectID, commit, taskID); err != nil {
			// Logged rather than raised. If the runner needs something it will
			// ask, and that question reaches Attention like any other; this is
			// for the case where it could not even start.
			log.Warn("could not start the preview of a finished task",
				"project", projectID, "task", taskID, "err", err)
		}
	}()
}
