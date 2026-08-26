package claudeharness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Checks are the probes only this adapter can perform. Each one exists because
// its absence cost real time: a CLI too old for its model, a config file that
// no longer parsed, an unanswered trust dialog. All three presented the same
// way — an agent that looked alive and did nothing.
func (a *Adapter) Checks() []adapter.Check {
	return []adapter.Check{
		adapter.BinaryPresent(binary),
		a.versionCheck(),
		a.configParsesCheck(),
		a.workspaceTrustedCheck(),
		a.modelAvailableCheck(),
	}
}

func (*Adapter) versionCheck() adapter.Check {
	return adapter.Check{
		Name: "binary_version",
		Run: func(ctx adapter.Ctx, _ adapter.Spec) adapter.Result {
			out, err := exec.CommandContext(ctx, binary, "--version").Output()
			if err != nil {
				return adapter.Result{
					Reason: fmt.Sprintf("%s --version failed: %v", binary, err),
					Remedy: "reinstall the claude CLI",
				}
			}
			v := strings.TrimSpace(string(out))
			if v == "" {
				return adapter.Result{
					Reason: binary + " reported no version",
					Remedy: "reinstall the claude CLI",
				}
			}
			return adapter.Result{OK: true, Detail: v}
		},
	}
}

// configParsesCheck reads the CLI's own configuration before trusting it.
//
// Two agents racing a read-modify-write of one global config file left it
// holding three concatenated copies of itself, and every invocation on that
// machine then failed to parse it — including for unrelated projects. Parsing
// it first turns that from a mystery into a sentence.
func (*Adapter) configParsesCheck() adapter.Check {
	return adapter.Check{
		Name: "config_parses",
		Run: func(_ adapter.Ctx, spec adapter.Spec) adapter.Result {
			for _, p := range configPaths(spec) {
				raw, err := os.ReadFile(p)
				if os.IsNotExist(err) {
					continue // absent is fine; the CLI writes it on first use
				}
				if err != nil {
					return adapter.Result{
						Reason: fmt.Sprintf("cannot read %s: %v", p, err),
						Remedy: "check the file's permissions",
					}
				}
				if !json.Valid(raw) {
					return adapter.Result{
						Reason: fmt.Sprintf("%s is not valid JSON", p),
						Remedy: fmt.Sprintf("repair or remove %s; the CLI recreates it", p),
					}
				}
			}
			return adapter.Result{OK: true, Detail: "config parses"}
		},
	}
}

// workspaceTrustedCheck looks for the dialog that once silently blocked four
// roles at once.
//
// Headless runs do not hit the interactive prompt, so this is a warning rather
// than a block: it matters for takeover (§10.1), where a real TUI attaches and
// a human meets the dialog with no idea why the agent stopped.
func (*Adapter) workspaceTrustedCheck() adapter.Check {
	return adapter.Check{
		Name: "workspace_trusted",
		Run: func(_ adapter.Ctx, spec adapter.Spec) adapter.Result {
			path, err := userConfigJSON()
			if err != nil {
				return adapter.Result{OK: true, Detail: "no config to consult"}
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return adapter.Result{OK: true, Detail: "no config to consult"}
			}
			var doc struct {
				Projects map[string]struct {
					HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			if json.Unmarshal(raw, &doc) != nil {
				// configParsesCheck reports malformed config; do not duplicate it.
				return adapter.Result{OK: true, Detail: "config unreadable, reported separately"}
			}

			wt, err := filepath.Abs(spec.Worktree)
			if err != nil {
				wt = spec.Worktree
			}
			if p, ok := doc.Projects[wt]; ok && p.HasTrustDialogAccepted {
				return adapter.Result{OK: true, Detail: "workspace trusted"}
			}
			return adapter.Result{
				Warn:   true,
				Reason: fmt.Sprintf("%s has not been trusted in claude", wt),
				Remedy: "headless runs are unaffected; run claude once in that directory before using takeover",
			}
		},
	}
}

// modelAvailableCheck warns about an unlisted model without blocking on it.
//
// A catalog can lag a model that works, so blocking would be wrong. Saying
// nothing would also be wrong: a mistyped id is how an agent ends up returning
// HTTP 400 on every turn for twenty minutes while looking perfectly healthy.
func (a *Adapter) modelAvailableCheck() adapter.Check {
	return adapter.Check{
		Name: "model_available",
		Run: func(ctx adapter.Ctx, spec adapter.Spec) adapter.Result {
			if spec.Model == "" {
				return adapter.Result{OK: true, Detail: "harness default"}
			}
			models, err := a.ListModels(ctx)
			if err != nil {
				return adapter.Result{OK: true, Detail: "catalog unavailable"}
			}
			for _, m := range models {
				if m.ID == spec.Model {
					return adapter.Result{OK: true, Detail: m.ID}
				}
			}
			return adapter.Result{
				Warn:   true,
				Reason: fmt.Sprintf("%q is not in this CLI's model list", spec.Model),
				Remedy: "it may still work, since the list is not exhaustive, but check the spelling",
			}
		},
	}
}

// configPaths are the files the CLI reads, per-role first when a private config
// directory is in play.
func configPaths(spec adapter.Spec) []string {
	if spec.ConfigDir != "" {
		return []string{
			filepath.Join(spec.ConfigDir, ".claude.json"),
			filepath.Join(spec.ConfigDir, "settings.json"),
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
	}
}
