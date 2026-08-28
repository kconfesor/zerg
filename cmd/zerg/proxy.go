package main

import (
	"log/slog"

	"github.com/kconfesor/zerg/internal/api"
	"github.com/kconfesor/zerg/internal/store"
)

// newViewer builds the thing that puts a running service on an origin of its
// own, next to the cockpit.
//
// One origin per service, opened when somebody first asks for the link; see
// internal/api/proxy.go for why it is not one shared origin with the service
// in the path. Bound on the same interface as the cockpit, because a preview
// reachable only from the daemon's own machine is no use to the phone reading
// the approval, and given the cockpit's certificate so a tailnet preview is
// https for a name the browser already trusts.
func newViewer(db *store.DB, cockpitAddr string, log *slog.Logger) *api.Viewer {
	return api.NewViewer(db, cockpitAddr, log).WithTouch(func(projectID string) {
		// Looked up when a request arrives rather than captured: the runner is
		// built after this, and the alternative is ordering the two by an
		// accident of the file.
		if runners != nil {
			runners.Touch(projectID)
		}
	})
}
