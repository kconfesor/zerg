package event

import (
	"sync"
	"testing"
	"time"

	"github.com/konfessor/zerg/internal/adapter"
)

func ev(text string) Event {
	return Event{Event: adapter.Event{Kind: adapter.EventMessage, Text: text}}
}

func TestEverySubscriberSeesEveryEvent(t *testing.T) {
	bus := NewBus()
	a, cancelA := bus.Subscribe(8)
	defer cancelA()
	b, cancelB := bus.Subscribe(8)
	defer cancelB()

	bus.Publish(ev("one"))

	for i, ch := range []<-chan Event{a, b} {
		select {
		case got := <-ch:
			if got.Text != "one" {
				t.Errorf("subscriber %d got %q", i, got.Text)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

// A browser that stops reading must never stall the agent producing events.
// Dropping is the deliberate trade: an observability gap beats a liveness one.
func TestASlowSubscriberDoesNotBlockPublishers(t *testing.T) {
	bus := NewBus()
	_, cancel := bus.Subscribe(2) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish(ev("flood"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
}

// One stalled subscriber must not cost the others their events.
func TestOneStalledSubscriberDoesNotStarveAnother(t *testing.T) {
	bus := NewBus()
	_, cancelSlow := bus.Subscribe(1) // never drained
	defer cancelSlow()
	fast, cancelFast := bus.Subscribe(64)
	defer cancelFast()

	for i := 0; i < 20; i++ {
		bus.Publish(ev("x"))
	}
	if got := len(fast); got != 20 {
		t.Errorf("the healthy subscriber received %d of 20 events", got)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe(4)

	bus.Publish(ev("before"))
	cancel()

	if bus.Subscribers() != 0 {
		t.Errorf("subscriber count = %d after cancel, want 0", bus.Subscribers())
	}
	// Publishing after cancel must not panic on a closed channel.
	bus.Publish(ev("after"))

	// The buffered event is still readable; the channel is then closed.
	if got := <-ch; got.Text != "before" {
		t.Errorf("got %q, want the event published before cancelling", got.Text)
	}
	if _, open := <-ch; open {
		t.Error("the channel should be closed after cancelling")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	bus := NewBus()
	_, cancel := bus.Subscribe(1)
	cancel()
	cancel() // must not panic by closing twice
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	bus := NewBus()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				bus.Publish(ev("concurrent"))
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := bus.Subscribe(16)
			defer cancel()
			timeout := time.After(500 * time.Millisecond)
			for {
				select {
				case <-ch:
				case <-timeout:
					return
				}
			}
		}()
	}
	wg.Wait()

	if n := bus.Subscribers(); n != 0 {
		t.Errorf("%d subscribers leaked", n)
	}
}
