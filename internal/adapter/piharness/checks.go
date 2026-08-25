package piharness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/konfessor/zerg/internal/adapter"
)

// Checks probe what only this adapter knows. Two of them exist because pi
// failed in exactly these ways on this machine: a credential missing for the
// selected provider, and an extension tree whose every entry failed to load.
func (a *Adapter) Checks() []adapter.Check {
	return []adapter.Check{
		adapter.BinaryPresent(binary),
		a.versionCheck(),
		a.configParsesCheck(),
		a.credentialsCheck(),
		a.modelAvailableCheck(),
	}
}

func (*Adapter) versionCheck() adapter.Check {
	return adapter.Check{
		Name: "binary_version",
		Run: func(ctx adapter.Ctx, _ adapter.Spec) adapter.Result {
			// pi prints its version on stderr, so stdout alone comes back empty
			// and the check reports "no version" for a perfectly healthy install.
			out, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
			if err != nil {
				return adapter.Result{
					Reason: fmt.Sprintf("%s --version failed: %v", binary, err),
					Remedy: "reinstall pi",
				}
			}
			v := strings.TrimSpace(string(out))
			if v == "" {
				return adapter.Result{Reason: "pi reported no version", Remedy: "reinstall pi"}
			}
			return adapter.Result{OK: true, Detail: v}
		},
	}
}

func (*Adapter) configParsesCheck() adapter.Check {
	return adapter.Check{
		Name: "config_parses",
		Run: func(_ adapter.Ctx, spec adapter.Spec) adapter.Result {
			dir := configDirFor(spec)
			if dir == "" {
				return adapter.Result{OK: true, Detail: "no config directory"}
			}
			for _, name := range []string{"settings.json", "auth.json", "models-store.json"} {
				p := filepath.Join(dir, name)
				raw, err := os.ReadFile(p)
				if os.IsNotExist(err) {
					continue
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
						Remedy: fmt.Sprintf("repair or remove %s; pi recreates it", p),
					}
				}
			}
			return adapter.Result{OK: true, Detail: "config parses"}
		},
	}
}

// credentialsCheck reports whether the provider this role's model names has
// credentials stored.
//
// zerg never logs anyone in and never touches an auth file — provider setup is
// explicitly out of scope. Detection is not: "No API key found for openai" is a
// sentence pi prints after it has already been handed work, and turning that
// into a blocked role with a remedy is the entire point of preflight.
func (*Adapter) credentialsCheck() adapter.Check {
	return adapter.Check{
		Name: "auth_valid",
		Run: func(_ adapter.Ctx, spec adapter.Spec) adapter.Result {
			provider, _, ok := strings.Cut(spec.Model, "/")
			if !ok || provider == "" {
				// Without a provider prefix pi falls back to its configured
				// default, which this check cannot resolve.
				return adapter.Result{OK: true, Detail: "provider not named by the model id"}
			}

			dir := configDirFor(spec)
			if dir == "" {
				return adapter.Result{OK: true, Detail: "no config directory to consult"}
			}
			raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
			if err != nil {
				return adapter.Result{
					Warn:   true,
					Reason: fmt.Sprintf("pi has no stored credentials for %q", provider),
					Remedy: "run pi and use /login for that provider",
				}
			}

			// Only key presence is read. The values are credentials and are
			// never logged, surfaced, or copied anywhere.
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil {
				return adapter.Result{OK: true, Detail: "credential store unreadable, reported separately"}
			}
			if _, found := doc[provider]; found {
				return adapter.Result{OK: true, Detail: "credentials present for " + provider}
			}
			// An env-var key is equally valid and lives outside this file.
			if envKeyFor(provider) {
				return adapter.Result{OK: true, Detail: "credentials supplied by environment"}
			}
			return adapter.Result{
				Warn:   true,
				Reason: fmt.Sprintf("no stored credentials for provider %q", provider),
				Remedy: "run pi and use /login for that provider, or export its API key",
			}
		},
	}
}

// envKeyFor reports whether an API key for the provider is in the environment.
// It checks presence only and never reads a value.
func envKeyFor(provider string) bool {
	candidates := map[string][]string{
		"openai":       {"OPENAI_API_KEY"},
		"openai-codex": {"OPENAI_API_KEY"},
		"anthropic":    {"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN"},
		"google":       {"GEMINI_API_KEY"},
		"groq":         {"GROQ_API_KEY"},
		"openrouter":   {"OPENROUTER_API_KEY"},
		"mistral":      {"MISTRAL_API_KEY"},
		"deepseek":     {"DEEPSEEK_API_KEY"},
	}
	for _, name := range candidates[provider] {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// modelAvailableCheck warns about an unlisted model without blocking.
//
// gpt-5.6-sol is the case in hand: absent from pi's own table, warned about by
// pi itself, and working. Blocking would be wrong; saying nothing would let a
// typo become twenty minutes of an agent looking alive while every turn fails.
func (a *Adapter) modelAvailableCheck() adapter.Check {
	return adapter.Check{
		Name: "model_available",
		Run: func(ctx adapter.Ctx, spec adapter.Spec) adapter.Result {
			if spec.Model == "" {
				return adapter.Result{OK: true, Detail: "harness default"}
			}
			models, err := a.ListModels(ctx)
			if err != nil || len(models) == 0 {
				return adapter.Result{OK: true, Detail: "catalog unavailable"}
			}
			for _, m := range models {
				if m.ID == spec.Model {
					return adapter.Result{OK: true, Detail: m.ID}
				}
			}
			return adapter.Result{
				Warn:   true,
				Reason: fmt.Sprintf("%q is not in pi's model table", spec.Model),
				Remedy: "it may still work — pi accepts custom model ids — but check the spelling",
			}
		},
	}
}

func configDirFor(spec adapter.Spec) string {
	if spec.ConfigDir != "" {
		return spec.ConfigDir
	}
	return userConfigDir()
}
