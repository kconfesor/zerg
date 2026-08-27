package main

import (
	"testing"

	"github.com/kconfesor/zerg/internal/store"
)

// The daemon has to answer to the URL it prints.
//
// It did not: the banner probed tailscale for the MagicDNS name while the guard
// was given cfg.TailnetHost, which is empty on every installation that never
// set it by hand. The cockpit's own tailnet address came back 403, saying the
// daemon does not answer to a name it had printed four lines earlier.
func TestListenerCarriesTheNameTheDaemonPrints(t *testing.T) {
	const probed = "kelvins-machine.tailnet.ts.net"

	cases := []struct {
		name        string
		cfg         store.Config
		probe       func() string
		wantTailnet string
		wantProbed  bool
	}{
		{
			name:        "serving tailscale TLS with nothing configured",
			cfg:         store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale},
			probe:       func() string { return probed },
			wantTailnet: probed,
			wantProbed:  true,
		},
		{
			name: "a name already configured is not second-guessed",
			cfg: store.Config{
				Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale,
				TailnetHost: "configured.tailnet.ts.net",
			},
			probe:       func() string { return probed },
			wantTailnet: "configured.tailnet.ts.net",
		},
		{
			name:        "no tailscale TLS, so no name and no probe",
			cfg:         store.Config{Addr: "127.0.0.1:7717", TLSMode: store.TLSOff},
			probe:       func() string { return probed },
			wantTailnet: "",
		},
		{
			name:        "tailscale unavailable",
			cfg:         store.Config{Addr: "100.64.0.1:7717", TLSMode: store.TLSTailscale},
			probe:       func() string { return "" },
			wantTailnet: "",
			wantProbed:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			got := listenerFor(tc.cfg, func() string {
				probed = true
				return tc.probe()
			})
			if got.TailnetHost != tc.wantTailnet {
				t.Errorf("TailnetHost = %q, want %q", got.TailnetHost, tc.wantTailnet)
			}
			if probed != tc.wantProbed {
				t.Errorf("probed = %v, want %v; probing shells out to tailscale", probed, tc.wantProbed)
			}
			// The rest of the listener is the configuration as stored.
			if got.Addr != tc.cfg.Addr {
				t.Errorf("Addr = %q, want %q", got.Addr, tc.cfg.Addr)
			}
		})
	}
}
