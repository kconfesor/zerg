package piharness

import (
	"context"
	"testing"

	"github.com/konfessor/zerg/internal/adapter"
)

// Runs the check against whatever this machine actually has, and prints it.
// Not an assertion: the result depends on the developer's node and extensions,
// and a test that failed because someone switched node version would be noise.
func TestExtensionsLoadableAgainstThisMachine(t *testing.T) {
	for _, c := range New().Checks() {
		if c.Name != "extensions_loadable" {
			continue
		}
		r := c.Run(context.Background(), adapter.Spec{})
		t.Logf("ok=%v warn=%v detail=%q reason=%q remedy=%q", r.OK, r.Warn, r.Detail, r.Reason, r.Remedy)
		if !r.OK && !r.Warn && r.Reason == "" {
			t.Error("a failing check must say what is wrong")
		}
	}
}
