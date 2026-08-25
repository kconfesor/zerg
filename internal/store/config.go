package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// SettingConfig holds the one document under which everything the UI can change
// about the daemon itself is stored.
//
// One row rather than a key per field: these are read together, written
// together from one form, and a half-applied network change is not a state
// worth being able to represent.
const SettingConfig = "config"

// TLSMode is where the certificate comes from.
const (
	// TLSOff serves plain HTTP. Correct on loopback, and the default.
	TLSOff = "off"

	// TLSTailscale asks the local tailscaled for a certificate for this
	// machine's MagicDNS name. The cert is a real one, so a phone on the
	// tailnet gets no warning — which matters more than it sounds: a browser
	// that has been taught to click through a warning on this page has been
	// taught to click through it everywhere.
	TLSTailscale = "tailscale"

	// TLSFiles uses a certificate and key already on disk.
	TLSFiles = "files"
)

// Cleanup policies for role worktrees.
const (
	// CleanNever leaves every worktree exactly as the agent left it.
	CleanNever = "never"

	// CleanOnDone removes ignored files from a role's worktree when a task it
	// worked reaches Done.
	CleanOnDone = "on_done"

	// CleanOnStart does the same sweep when the daemon starts, which catches
	// whatever the last run left behind after a crash.
	CleanOnStart = "on_start"
)

// Config is the daemon's own settings.
type Config struct {
	// Addr is host:port to serve the cockpit on. Loopback keeps the trust
	// boundary the same as the shell that launched it; anything else does not,
	// and the daemon says so at startup.
	Addr string `json:"addr"`

	TLSMode  string `json:"tlsMode"`
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`

	// TailnetHost is the MagicDNS name to request a certificate for. Empty
	// means ask tailscaled what this machine is called.
	TailnetHost string `json:"tailnetHost,omitempty"`

	// EventRetentionDays is how long a transcript stays replayable. Costs and
	// outcomes live in usage_turns and tasks and are never swept, so this
	// trades narrative for disk and nothing else.
	EventRetentionDays int `json:"eventRetentionDays"`

	// CleanPolicy says when to sweep role worktrees, and CleanIgnored says
	// what. Ignored files are the whole of the problem in practice: a Rust
	// calculator's checkout is 256 KB of source and 45 MB of target/, per role.
	CleanPolicy  string `json:"cleanPolicy"`
	CleanIgnored bool   `json:"cleanIgnored"`

	// PruneMergedBranches removes zerg-<role> branches whose work is already on
	// the base branch. They are cheap, but they accumulate forever and make
	// `git branch` in the operator's own repository useless.
	PruneMergedBranches bool `json:"pruneMergedBranches"`
}

// DefaultConfig is what a daemon runs with before anyone opens settings.
//
// Every default is the conservative one: loopback, no TLS, and no sweeping.
// Deleting files a user did not ask to have deleted is the kind of helpfulness
// that costs someone their afternoon, so cleanup is opt-in even though the
// disk numbers argue loudly for it.
func DefaultConfig() Config {
	return Config{
		Addr:                "127.0.0.1:7717",
		TLSMode:             TLSOff,
		EventRetentionDays:  14,
		CleanPolicy:         CleanNever,
		CleanIgnored:        true,
		PruneMergedBranches: false,
	}
}

// GetConfig reads the stored settings, filling anything absent from the
// defaults. A field added in a later version is therefore not a missing value
// on an existing install.
func (db *DB) GetConfig(ctx context.Context) (Config, error) {
	cfg := DefaultConfig()
	raw, err := db.GetSetting(ctx, SettingConfig)
	if err != nil || raw == "" {
		// Not set yet is the ordinary case on a fresh install.
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("stored settings are unreadable: %w", err)
	}
	return cfg, nil
}

// SetConfig validates and stores settings.
//
// Validation happens here rather than in the handler because these are also
// read at startup, and a value that cannot work should be rejected when it is
// written — not discovered when the daemon next fails to bind.
func (db *DB) SetConfig(ctx context.Context, cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	if err := db.SetSetting(ctx, SettingConfig, string(raw)); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate reports what is wrong with a configuration, in the words of someone
// who has to fix it.
func (cfg Config) Validate() error {
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return invalid("address must be host:port, like 127.0.0.1:7717 — %v", err)
	}

	switch cfg.TLSMode {
	case TLSOff, TLSTailscale:
	case TLSFiles:
		if strings.TrimSpace(cfg.CertFile) == "" || strings.TrimSpace(cfg.KeyFile) == "" {
			return invalid("serving TLS from files needs both a certificate and a key")
		}
	default:
		return invalid("unknown TLS mode %q; use off, tailscale or files", cfg.TLSMode)
	}

	if cfg.EventRetentionDays < 1 || cfg.EventRetentionDays > 3650 {
		return invalid("keep transcripts for between 1 and 3650 days")
	}

	switch cfg.CleanPolicy {
	case CleanNever, CleanOnDone, CleanOnStart:
	default:
		return invalid("unknown cleanup policy %q; use never, on_done or on_start", cfg.CleanPolicy)
	}
	return nil
}

// Retention is the transcript window as a duration.
func (cfg Config) Retention() time.Duration {
	return time.Duration(cfg.EventRetentionDays) * 24 * time.Hour
}

// LoopbackOnly reports whether the cockpit is reachable only from this machine.
func (cfg Config) LoopbackOnly() bool {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost", "":
		return true
	}
	return false
}
