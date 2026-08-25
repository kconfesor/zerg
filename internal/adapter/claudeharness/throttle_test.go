package claudeharness

import (
	"testing"
	"time"
)

// The strings here were read out of the claude binary, not recalled. Its own
// fatal-error classifier carries "usage limit reached"; the surrounding copy
// is "resets in <duration>" or "until your limit resets at <time>".
func TestRecognisesTheSubscriptionWindowBeingSpent(t *testing.T) {
	a := New()
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"plain", "Claude usage limit reached", true},
		{"with reset", "Usage limit reached · continuing automatically, resets in 42 minutes", true},
		{"absolute", "Paused until your limit resets at 3pm", true},
		{"api error type", `{"type":"rate_limit_error","message":"..."}`, true},

		// Everything that is not a quota limit must stay fatal, or a real
		// failure becomes an agent that waits forever for nothing.
		{"auth", "invalid api key", false},
		{"credit", "credit balance is too low", false},
		{"crash", "panic: runtime error: index out of range", false},
		// Informational copy the binary also carries. Matching a bare
		// "limit resets" here would pause an agent that is working fine.
		{"lower-priority notice", "Lower-priority mode is offered again after your weekly limit resets.", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := a.ThrottledBy(tc.text); ok != tc.want {
				t.Errorf("ThrottledBy(%q) = %v, want %v", tc.text, ok, tc.want)
			}
		})
	}
}

func TestReadsWhenTheWindowLifts(t *testing.T) {
	now := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name, text string
		want       time.Time
	}{
		{"relative minutes", "usage limit reached, resets in 42 minutes", now.Add(42 * time.Minute)},
		{"relative hours", "usage limit reached, try again in ~3 hours", now.Add(3 * time.Hour)},
		{"absolute pm", "Paused until your limit resets at 6pm", now.Add(4 * time.Hour)},
		{"absolute with minutes", "usage limit reached, resets at 14:30", now.Add(30 * time.Minute)},

		// A time already past today means tomorrow. Without that, the role
		// resumes instantly and walks straight back into the wall.
		{"already past", "usage limit reached, resets at 9am", now.Add(19 * time.Hour)},

		// No stated time is normal, and must not be guessed at.
		{"unstated", "usage limit reached", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeResetAt(tc.text, now)
			if !got.Equal(tc.want) {
				t.Errorf("claudeResetAt(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
