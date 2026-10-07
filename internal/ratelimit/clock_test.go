package ratelimit

import (
	"testing"
	"time"
)

// fakeClock is advanced by hand (PLAN-002 8.5): no sleeping in tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)} }

func TestDeduplicator_TTLExpiresOnClock(t *testing.T) {
	c := newClock()
	d := NewDeduplicator(time.Minute)
	defer d.Stop()
	d.now = c.now
	if !d.Check("k") || d.Check("k") {
		t.Fatal("first Check passes, a repeat within the TTL is suppressed")
	}
	c.advance(59 * time.Second)
	if d.Check("k") {
		t.Fatal("still within the TTL")
	}
	c.advance(2 * time.Second)
	if !d.Check("k") {
		t.Fatal("after the TTL the key passes again")
	}
}

func TestActionLimiter_WindowRollsOver(t *testing.T) {
	c := newClock()
	l := NewActionLimiter(2, 10*time.Minute)
	l.now = c.now
	got := []bool{l.Allow(), l.Allow(), l.Allow()}
	if !got[0] || !got[1] || got[2] {
		t.Fatalf("two actions allowed, the third refused; got %v", got)
	}
	c.advance(10*time.Minute + time.Second)
	if !l.Allow() {
		t.Fatal("budget returns once the window has passed")
	}
}

func TestCircuitBreaker_TripsAndRecovers(t *testing.T) {
	c := newClock()
	cb := NewCircuitBreaker(2, time.Hour)
	cb.now = c.now
	if !cb.RecordAndCheck("ns", "api") {
		t.Fatal("first action allowed")
	}
	if cb.RecordAndCheck("ns", "api") || !cb.IsTripped("ns", "api") {
		t.Fatal("second action reaches the threshold and trips")
	}
	if cb.IsTripped("ns", "other") {
		t.Fatal("other workloads are not affected")
	}
	c.advance(59 * time.Minute)
	if cb.RecordAndCheck("ns", "api") {
		t.Fatal("still tripped inside the window")
	}
	c.advance(2 * time.Minute)
	if cb.IsTripped("ns", "api") || !cb.RecordAndCheck("ns", "api") {
		t.Fatal("after the window the breaker resets")
	}
}

func TestNewCircuitBreaker_Defaults(t *testing.T) {
	cb := NewCircuitBreaker(0, 0)
	if cb.threshold != 5 || cb.window != time.Hour {
		t.Fatalf("defaults: threshold %d window %s", cb.threshold, cb.window)
	}
}

func TestActionLimiter_Remaining(t *testing.T) {
	c := newClock()
	l := NewActionLimiter(2, time.Minute)
	l.now = c.now
	if l.Remaining() != 2 {
		t.Fatal("full budget at start")
	}
	l.Allow()
	if l.Remaining() != 1 {
		t.Fatal("one used")
	}
	l.events = append(l.events, c.t, c.t) // more events than max cannot go negative
	if l.Remaining() != 0 {
		t.Fatal("never negative")
	}
}
