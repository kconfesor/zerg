package piharness

import (
	"testing"
	"time"
)

// pi's Codex provider composes this sentence from the error's plan_type and
// resets_at; the codes are what appear when a raw error body reaches the
// stream. Both were read from its openai-codex-responses.js.
func TestRecognisesTheChatGPTQuotaBeingSpent(t *testing.T) {
	a := New()
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"pi's own sentence", "You have hit your ChatGPT usage limit (plus plan). Try again in ~47 min.", true},
		{"raw code", `{"error":{"code":"usage_limit_reached"}}`, true},
		{"not included", `{"error":{"code":"usage_not_included"}}`, true},
		{"rate limited", `{"error":{"code":"rate_limit_exceeded"}}`, true},

		{"auth", "No API key found for openai", false},
		{"crash", "Cannot find module '.../pi-ai/dist/index.js/compat'", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := a.ThrottledBy(tc.text); ok != tc.want {
				t.Errorf("ThrottledBy(%q) = %v, want %v", tc.text, ok, tc.want)
			}
		})
	}
}

func TestPrefersTheExactResetStampOverPisRounding(t *testing.T) {
	now := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	exact := now.Add(47*time.Minute + 30*time.Second)

	// pi rounds resets_at to "~47 min" for its message. When both are present
	// the stamp wins, because the sentence was derived from it.
	text := `{"error":{"code":"usage_limit_reached","resets_at":` +
		itoa(exact.Unix()) + `}} You have hit your ChatGPT usage limit. Try again in ~47 min.`

	if got := piResetAt(text, now); !got.Equal(exact.Truncate(time.Second)) {
		t.Errorf("piResetAt = %v, want the exact stamp %v", got, exact)
	}

	// With only the sentence, the rounded value is all there is.
	only := "You have hit your ChatGPT usage limit (plus plan). Try again in ~47 min."
	if got := piResetAt(only, now); !got.Equal(now.Add(47 * time.Minute)) {
		t.Errorf("piResetAt(sentence only) = %v, want %v", got, now.Add(47*time.Minute))
	}

	// A stamp already in the past is ignored rather than resuming instantly.
	stale := `{"error":{"code":"usage_limit_reached","resets_at":` + itoa(now.Add(-time.Hour).Unix()) + `}}`
	if got := piResetAt(stale, now); !got.IsZero() {
		t.Errorf("piResetAt(stale stamp) = %v, want zero", got)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
