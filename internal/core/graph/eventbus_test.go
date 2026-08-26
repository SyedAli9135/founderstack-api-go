package graph

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEventBus_PublishDeliversToSubscriber(t *testing.T) {
	bus := NewEventBus()
	runID := uuid.New()

	ch, unsubscribe := bus.Subscribe(runID)
	defer unsubscribe()

	bus.Publish(Event{Type: EventNodeStart, RunID: runID, Data: "planner"})

	select {
	case ev := <-ch:
		if ev.Type != EventNodeStart || ev.Data != "planner" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		if ev.Timestamp.IsZero() {
			t.Fatal("expected Timestamp to be filled in")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

func TestEventBus_PublishOnlyReachesMatchingRunID(t *testing.T) {
	bus := NewEventBus()
	runA, runB := uuid.New(), uuid.New()

	chA, unsubA := bus.Subscribe(runA)
	defer unsubA()
	chB, unsubB := bus.Subscribe(runB)
	defer unsubB()

	bus.Publish(Event{Type: EventComplete, RunID: runA})

	select {
	case <-chA:
	case <-time.After(time.Second):
		t.Fatal("runA subscriber never received its event")
	}

	select {
	case ev := <-chB:
		t.Fatalf("runB subscriber should not have received runA's event, got %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := NewEventBus()
	runID := uuid.New()

	ch, unsubscribe := bus.Subscribe(runID)
	unsubscribe()

	// Publishing after unsubscribe must not panic (closed channel) and must
	// not be observable on ch.
	bus.Publish(Event{Type: EventError, RunID: runID})

	if _, open := <-ch; open {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
}

func TestEventBus_PublishNeverBlocksOnFullSubscriber(t *testing.T) {
	bus := NewEventBus()
	runID := uuid.New()

	_, unsubscribe := bus.Subscribe(runID) // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish(Event{Type: EventToken, RunID: runID})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full, undrained subscriber channel")
	}
}
