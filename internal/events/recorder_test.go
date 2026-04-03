package events

import (
	"testing"
	"time"
)

func TestRecorder_Basic(t *testing.T) {
	r := NewRecorder(5)

	r.Record(Event{Reason: "crash1", Type: Incident})
	r.Record(Event{Reason: "crash2", Type: Incident})
	r.Record(Event{Reason: "scale1", Type: Scaling})

	if r.Count() != 3 {
		t.Fatalf("expected 3, got %d", r.Count())
	}

	recent := r.Recent(2)
	if len(recent) != 2 {
		t.Fatalf("expected 2 recent, got %d", len(recent))
	}
	if recent[0].Reason != "scale1" {
		t.Errorf("expected newest first, got %s", recent[0].Reason)
	}
	if recent[1].Reason != "crash2" {
		t.Errorf("expected second newest, got %s", recent[1].Reason)
	}
}

func TestRecorder_RingBuffer(t *testing.T) {
	r := NewRecorder(3)

	r.Record(Event{Reason: "e1"})
	r.Record(Event{Reason: "e2"})
	r.Record(Event{Reason: "e3"})
	r.Record(Event{Reason: "e4"}) // should drop e1

	if r.Count() != 3 {
		t.Fatalf("expected 3, got %d", r.Count())
	}

	recent := r.Recent(0) // 0 means all
	if recent[0].Reason != "e4" {
		t.Errorf("expected e4, got %s", recent[0].Reason)
	}
	if recent[2].Reason != "e2" {
		t.Errorf("expected e2 (oldest), got %s", recent[2].Reason)
	}
}

func TestRecorder_Since(t *testing.T) {
	r := NewRecorder(10)

	past := time.Now().Add(-5 * time.Minute)
	r.Record(Event{Reason: "old", Timestamp: past})
	r.Record(Event{Reason: "new"}) // defaults to now

	since := r.Since(time.Now().Add(-1 * time.Minute))
	if len(since) != 1 {
		t.Fatalf("expected 1, got %d", len(since))
	}
	if since[0].Reason != "new" {
		t.Errorf("expected 'new', got %s", since[0].Reason)
	}
}

func TestRecorder_Stats(t *testing.T) {
	r := NewRecorder(10)

	r.Record(Event{Type: Incident})
	r.Record(Event{Type: Incident})
	r.Record(Event{Type: Action})
	r.Record(Event{Type: Scaling})

	stats := r.Stats()
	if stats[Incident] != 2 {
		t.Errorf("expected 2 incidents, got %d", stats[Incident])
	}
	if stats[Action] != 1 {
		t.Errorf("expected 1 action, got %d", stats[Action])
	}
}

func TestRecorder_IDs(t *testing.T) {
	r := NewRecorder(10)
	r.Record(Event{Reason: "a"})
	r.Record(Event{Reason: "b"})
	r.Record(Event{Reason: "c"})

	recent := r.Recent(3)
	if recent[0].ID != 3 || recent[1].ID != 2 || recent[2].ID != 1 {
		t.Errorf("unexpected IDs: %d, %d, %d", recent[0].ID, recent[1].ID, recent[2].ID)
	}
}
