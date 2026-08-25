package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/konfessor/zerg/internal/hatchery"
	"github.com/konfessor/zerg/internal/store"
	"github.com/konfessor/zerg/internal/tailnet"
)

// settingsResponse is the settings form plus the facts it needs to be filled
// in sensibly: what the machine is called on the tailnet, and whether HTTPS is
// available there. Without those the TLS choice is a guess.
type settingsResponse struct {
	Config  store.Config   `json:"config"`
	Tailnet tailnet.Status `json:"tailnet"`

	// Applied is the address actually being served right now, which differs
	// from Config.Addr whenever settings have been saved but not restarted.
	Applied       string `json:"applied"`
	RestartNeeded bool   `json:"restartNeeded"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetConfig(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:        cfg,
		Tailnet:       tailnet.Probe(r.Context()),
		Applied:       s.applied,
		RestartNeeded: s.applied != "" && s.applied != cfg.Addr,
	})
}

// setSettings stores settings and says what still has to happen for them to
// take effect.
//
// Rebinding a live listener would drop every open connection, including the
// stream the caller is watching, so the address and TLS mode apply on restart
// and the response says so. Retention and cleanup apply immediately, because
// nothing is holding them.
func (s *Server) setSettings(w http.ResponseWriter, r *http.Request) {
	var cfg store.Config
	if !decode(w, r, &cfg) {
		return
	}
	saved, err := s.db.SetConfig(r.Context(), cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:        saved,
		Tailnet:       tailnet.Probe(r.Context()),
		Applied:       s.applied,
		RestartNeeded: s.applied != "" && s.applied != saved.Addr,
	})
}

// sweep reclaims disk for one project, on demand.
//
// Exposed as a button as well as a policy because "how much would this free"
// is a question best answered by doing it once and reading the number.
func (s *Server) sweep(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetConfig(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	freed, pruned, err := Sweep(r.Context(), s.db, r.PathValue("id"), cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bytesFreed":     freed,
		"branchesPruned": pruned,
	})
}

// Sweep reclaims disk for one project: ignored files out of every role's
// worktree, and optionally the role branches already merged into the base.
//
// Ignored files only, ever. A worktree is mostly regenerable bytes — 45 MB of
// build output against 256 KB of source, per role, in a real project — but an
// agent's uncommitted work lives in untracked files, and a cleanup that eats
// those is worse than a full disk.
func Sweep(ctx context.Context, db *store.DB, projectID string, cfg store.Config) (int64, []string, error) {
	project, err := db.GetProject(ctx, projectID)
	if err != nil {
		return 0, nil, err
	}
	team, err := db.ResolveTeam(ctx, projectID)
	if err != nil {
		return 0, nil, err
	}

	hat := hatchery.New(project.Path)
	var freed int64
	if cfg.CleanIgnored {
		for _, role := range team {
			n, err := hat.SweepIgnored(ctx, role.Name)
			if err != nil {
				return freed, nil, fmt.Errorf("sweeping %s: %w", role.Name, err)
			}
			freed += n
		}
	}

	var pruned []string
	if cfg.PruneMergedBranches {
		pruned, err = hat.PruneMergedBranches(ctx, project.BaseBranch)
		if err != nil {
			return freed, nil, err
		}
	}
	return freed, pruned, nil
}
