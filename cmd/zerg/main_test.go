package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kconfesor/zerg/internal/store"
	"github.com/kconfesor/zerg/internal/tailnet"
)

// The daemon has to answer to the URL it prints, and get its certificate for
// that same name.
//
// It did not: the banner probed tailscale for the MagicDNS name, the guard was
// given cfg.TailnetHost, which is empty on every installation that never set it
// by hand, and the certificate path probed a third time. The cockpit's own
// tailnet address came back 403, four lines under a banner advertising it.
func TestTailnetHostIsResolvedOnceForEveryoneWhoNeedsIt(t *testing.T) {
	const discovered = "kelvins-machine.tailnet.ts.net"
	up := tailnet.Status{Available: true, DNSName: discovered}
	down := tailnet.Status{Available: false, Reason: "tailscaled is not running"}

	cases := []struct {
		name       string
		cfg        store.Config
		probe      tailnet.Status
		wantHost   string
		wantProbed bool
	}{
		{
			name:       "tailscale TLS with nothing configured",
			cfg:        store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale},
			probe:      up,
			wantHost:   discovered,
			wantProbed: true,
		},
		{
			name: "a configured name is not second-guessed",
			cfg: store.Config{
				Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale,
				TailnetHost: "configured.tailnet.ts.net",
			},
			probe:    up,
			wantHost: "configured.tailnet.ts.net",
		},
		{
			// `zerg up --no-tls --addr $(tailscale ip -4):7717`. Reached by the
			// tailnet name even with TLS off, so the guard needs it.
			name:       "no TLS but bound where the tailnet can reach it",
			cfg:        store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSOff},
			probe:      up,
			wantHost:   discovered,
			wantProbed: true,
		},
		{
			name:     "loopback with TLS off needs no name and no probe",
			cfg:      store.Config{Addr: "127.0.0.1:7717", TLSMode: store.TLSOff},
			probe:    up,
			wantHost: "",
		},
		{
			name:       "tailscale is not running",
			cfg:        store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale},
			probe:      down,
			wantHost:   "",
			wantProbed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			host, st := tailnetHostFor(tc.cfg, func() tailnet.Status {
				probed = true
				return tc.probe
			})
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if probed != tc.wantProbed {
				t.Errorf("probed = %v, want %v; probing shells out to tailscale", probed, tc.wantProbed)
			}
			// The status travels with it so the certificate path can say why
			// there is no name, rather than reporting an empty one.
			if tc.wantProbed && (host != "") != st.Available {
				t.Errorf("status = %+v, which does not match host %q", st, host)
			}
		})
	}
}

// The discovered name must not reach the saved configuration.
//
// Putting it in the listener made the settings view compare a discovered value
// against a configuration that never held one, so every tailscale installation
// reported "serving X until restart" from the moment it started, and restarting
// could not clear it.
func TestResolvingTheNameDoesNotChangeTheSavedListener(t *testing.T) {
	cfg := store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale}
	before := cfg.Listener()

	host, _ := tailnetHostFor(cfg, func() tailnet.Status {
		return tailnet.Status{Available: true, DNSName: "kelvins-machine.tailnet.ts.net"}
	})
	if host == "" {
		t.Fatal("no host resolved")
	}
	if after := cfg.Listener(); after != before {
		t.Errorf("listener changed from %+v to %+v; the saved configuration is what a restart is measured against", before, after)
	}
	if cfg.Listener().TailnetHost != "" {
		t.Error("the discovered name reached the listener, which is what made restartNeeded stick")
	}
}

// Deploying when a card lands is the card's decision.
//
// It was the project's: one checkbox meaning "preview everything here", which
// is either off and never used or on and paying an agent turn to preview a
// README fix. The card carries the answer now, and this is that answer being
// read back from the database rather than from the thing that wrote it.
func TestOnlyACardThatAskedIsDeployedWhenItLands(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	project, err := db.CreateProject(ctx, t.TempDir(), "Ledger", "main")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	asked, err := db.CreateTask(ctx, project.ID, "Add the settings page", "", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE tasks SET deploy = ? WHERE id = ?`, store.DeployLocal, asked.ID); err != nil {
		t.Fatal(err)
	}
	quiet, err := db.CreateTask(ctx, project.ID, "Fix a typo", "", "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if !deploysOnLanding(ctx, db, asked.ID, "abc1234") {
		t.Error("a card that asked to be deployed was not")
	}
	if deploysOnLanding(ctx, db, quiet.ID, "abc1234") {
		t.Error("a card that did not ask was deployed anyway, which is somebody's money")
	}
	// Rejected work, or work that changed nothing, leaves no commit and there
	// is nothing to put anywhere.
	if deploysOnLanding(ctx, db, asked.ID, "") {
		t.Error("a card with no commit was deployed")
	}
}
