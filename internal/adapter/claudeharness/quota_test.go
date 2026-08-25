package claudeharness

import (
	"testing"
	"time"

	"github.com/kconfesor/zerg/internal/adapter"
)

// Captured verbatim from `claude -p "say ok" --output-format stream-json`.
// This event rides the stream on every turn, which is what makes the gauge
// free — there is nothing to poll.
const rateLimitEventJSON = `{"type":"rate_limit_event","rate_limit_info":{` +
	`"status":"allowed","resetsAt":1787685600,"rateLimitType":"five_hour",` +
	`"overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{` +
	`"five_hour":{"utilization":0.39,"resetsAt":1787685600},` +
	`"seven_day":{"utilization":0.68,"resetsAt":1787709600}}},` +
	`"uuid":"ab46bc5d","session_id":"c0ce559f"}`

func TestReadsTheSubscriptionGaugeOffTheStream(t *testing.T) {
	evs, err := New().Parse([]byte(rateLimitEventJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != adapter.EventQuota {
		t.Fatalf("got %+v, want one quota event", evs)
	}
	q := evs[0].Quota
	if q == nil {
		t.Fatal("quota event carried no quota")
	}
	if len(q.Windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(q.Windows))
	}

	// Ordered shortest first, because a map's order is random and these are
	// rendered in sequence.
	if q.Windows[0].Window != 5*time.Hour || q.Windows[1].Window != 7*24*time.Hour {
		t.Errorf("windows out of order: %v, %v", q.Windows[0].Window, q.Windows[1].Window)
	}
	if q.Windows[0].Used != 0.39 || q.Windows[1].Used != 0.68 {
		t.Errorf("utilisation misread: %v, %v", q.Windows[0].Used, q.Windows[1].Used)
	}
	if got := q.Windows[0].ResetsAt.Unix(); got != 1787685600 {
		t.Errorf("resetsAt = %d, want 1787685600", got)
	}
	if l := q.Windows[1].Label(); l != "7d" {
		t.Errorf("label = %q, want 7d", l)
	}

	// The tightest window is the one that will actually stop work.
	w, ok := q.Tightest()
	if !ok || w.Used != 0.68 {
		t.Errorf("Tightest = %+v, want the 68%% weekly window", w)
	}
}

// An event with no windows must produce nothing rather than an empty gauge —
// a bar at zero and a bar that is unknown are different facts.
func TestAnEmptyRateLimitEventProducesNoGauge(t *testing.T) {
	evs, err := New().Parse([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("got %+v, want no events", evs)
	}
}
