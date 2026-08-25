package adapter

import (
	"slices"
	"strings"
	"testing"
)

// The daemon runs in whatever shell started it, and that shell routinely holds
// credentials for things an agent has no business reaching. Inheriting the
// whole environment handed every one of them to every agent.
func TestAgentsDoNotInheritUnrelatedCredentials(t *testing.T) {
	// Secrets that belong to the operator, not to a coding agent.
	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"DATABASE_URL", "STRIPE_SECRET_KEY", "KUBECONFIG", "NPM_TOKEN",
	} {
		t.Setenv(name, "secret")
	}
	// What a harness authenticates with, and what a process needs to run.
	t.Setenv("ANTHROPIC_API_KEY", "keep")
	t.Setenv("OPENAI_API_KEY", "keep")
	t.Setenv("PI_CODING_AGENT_DIR", "keep")
	t.Setenv("HOME", "/home/someone")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	env := AgentEnv(Spec{Role: "coder", Socket: "/tmp/s", Token: "t"})

	has := func(name string) bool {
		return slices.ContainsFunc(env, func(kv string) bool {
			return strings.HasPrefix(kv, name+"=")
		})
	}

	for _, name := range []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"DATABASE_URL", "STRIPE_SECRET_KEY", "KUBECONFIG", "NPM_TOKEN",
	} {
		if has(name) {
			t.Errorf("%s reached the agent", name)
		}
	}
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "PI_CODING_AGENT_DIR",
		"HOME", "LC_ALL", "PATH", "ZERG_SOCKET", "ZERG_TOKEN", "ZERG_ROLE",
	} {
		if !has(name) {
			t.Errorf("%s was withheld; the agent needs it", name)
		}
	}
}
