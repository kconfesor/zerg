package adapter

import (
	"context"
	"testing"
)

// A level from the wrong harness's list stops a role before it is spawned.
//
// Unlike a model, which is free text because a working id can be missing from
// any catalog, the levels are the CLI's own: claude refuses "off" and exits on
// its usage message, which reads as a role that will not start and says nothing
// about why. Readiness and the spawn guard run the same check.
func TestThinkingLevelIsCheckedAgainstTheHarness(t *testing.T) {
	check := ThinkingSupported([]string{"low", "medium", "high"})

	cases := []struct {
		level string
		ok    bool
	}{
		{"", true}, // the harness's own default
		{"high", true},
		{"off", false},  // pi has this level; claude does not
		{"HIGH", false}, // the CLI does not case-fold it either
	}
	for _, tc := range cases {
		res := check.Run(context.Background(), Spec{Thinking: tc.level})
		if res.OK != tc.ok {
			t.Errorf("level %q: OK = %v, want %v (%s)", tc.level, res.OK, tc.ok, res.Reason)
		}
		if !res.OK && res.Remedy == "" {
			t.Errorf("level %q was refused without saying what to do instead", tc.level)
		}
	}

	// A harness with no such control refuses any level rather than passing one
	// it would choke on.
	none := ThinkingSupported(nil)
	if res := none.Run(context.Background(), Spec{Thinking: "high"}); res.OK {
		t.Error("a level was accepted for a harness that takes none")
	}
	if res := none.Run(context.Background(), Spec{}); !res.OK {
		t.Errorf("a role that sets no level was refused: %s", res.Reason)
	}
}
