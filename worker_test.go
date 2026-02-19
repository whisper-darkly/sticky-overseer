package main

import (
	"testing"
	"time"
)

func newTestWorker() *Worker {
	return &Worker{
		PID:    1234,
		TaskID: "test-task",
		State:  "running",
	}
}

func TestAddEvent_RingBuffer_Eviction(t *testing.T) {
	w := newTestWorker()
	base := time.Now().UTC()

	// Fill to capacity
	for i := 0; i < ringBufferSize; i++ {
		w.addEvent(Event{Type: "output", Data: string(rune('A' + i%26)), TS: base.Add(time.Duration(i) * time.Millisecond)})
	}
	if len(w.events) != ringBufferSize {
		t.Fatalf("expected %d events, got %d", ringBufferSize, len(w.events))
	}

	// Add one more — oldest should be evicted
	w.addEvent(Event{Type: "output", Data: "EXTRA", TS: base.Add(ringBufferSize * time.Millisecond)})
	if len(w.events) != ringBufferSize {
		t.Fatalf("after eviction: expected %d events, got %d", ringBufferSize, len(w.events))
	}
	// Last event should be the new one
	if w.events[len(w.events)-1].Data != "EXTRA" {
		t.Errorf("last event data: got %q want EXTRA", w.events[len(w.events)-1].Data)
	}
	// First event should no longer be the original first
	if w.events[0].Data == string(rune('A')) {
		t.Error("first event was not evicted")
	}
}

func TestGetEvents_NoFilter_ReturnsCopy(t *testing.T) {
	w := newTestWorker()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		w.addEvent(Event{Type: "output", Data: "x", TS: now.Add(time.Duration(i) * time.Second)})
	}

	events := w.getEvents(nil)
	if len(events) != 5 {
		t.Fatalf("expected 5, got %d", len(events))
	}

	// Mutation of returned slice should not affect internal state
	events[0].Data = "mutated"
	if w.events[0].Data == "mutated" {
		t.Error("getEvents returned a direct reference, not a copy")
	}
}

func TestGetEvents_WithSinceFilter(t *testing.T) {
	w := newTestWorker()
	base := time.Now().UTC()
	for i := 0; i < 10; i++ {
		w.addEvent(Event{Type: "output", Data: "x", TS: base.Add(time.Duration(i) * time.Second)})
	}

	// Events at base+5s and later
	since := base.Add(5 * time.Second)
	events := w.getEvents(&since)
	if len(events) != 5 {
		t.Fatalf("expected 5 events at or after since, got %d", len(events))
	}
	for _, e := range events {
		if e.TS.Before(since) {
			t.Errorf("event %v is before since %v", e.TS, since)
		}
	}
}

func TestLastEventAt_Empty(t *testing.T) {
	w := newTestWorker()
	if w.lastEventAt() != nil {
		t.Error("expected nil for empty worker")
	}
}

func TestLastEventAt_NonEmpty(t *testing.T) {
	w := newTestWorker()
	t1 := time.Now().UTC()
	t2 := t1.Add(time.Second)
	w.addEvent(Event{Type: "output", TS: t1})
	w.addEvent(Event{Type: "output", TS: t2})

	got := w.lastEventAt()
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if !got.Equal(t2) {
		t.Errorf("lastEventAt: got %v want %v", *got, t2)
	}
}
