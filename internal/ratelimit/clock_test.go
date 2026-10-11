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

// A state that does not change by itself is reported once per its own
// window, independent of the default TTL.
func TestDeduplicator_CheckForOwnWindow(t *testing.T) {
	c := newClock()
	d := NewDeduplicator(time.Minute)
	defer d.Stop()
	d.now = c.now
	if !d.CheckFor("job", 24*time.Hour) || d.CheckFor("job", 24*time.Hour) {
		t.Fatal("first CheckFor passes, a repeat is suppressed")
	}
	c.advance(2 * time.Hour)
	if d.Check("job") || d.CheckFor("job", 24*time.Hour) {
		t.Fatal("the long window holds past the default TTL")
	}
	c.advance(23 * time.Hour)
	if !d.CheckFor("job", 24*time.Hour) {
		t.Fatal("after its window the key passes again")
	}
}

// D8: the circuit breaker's window ends exactly at window, in every method.
// Mutation testing found no test at the boundary.
func TestCircuitBreaker_WindowBoundary(t *testing.T) {
	c := newClock()
	cb := NewCircuitBreaker(2, time.Minute) // the second action in a window trips it
	cb.now = c.now
	if !cb.RecordAndCheck("ns", "api") {
		t.Fatal("the first action is allowed")
	}
	if cb.RecordAndCheck("ns", "api") {
		t.Fatal("the second action trips the breaker")
	}
	c.advance(time.Minute - time.Nanosecond)
	if !cb.IsTripped("ns", "api") || len(cb.TrippedWorkloads()) != 1 || cb.RecordAndCheck("ns", "api") {
		t.Fatal("one nanosecond before the window ends the breaker is still tripped")
	}
	c.advance(time.Nanosecond)
	if got := cb.TrippedWorkloads(); len(got) != 0 {
		t.Fatalf("at exactly the window it lists nothing, got %v", got)
	}
	if cb.IsTripped("ns", "api") {
		t.Fatal("at exactly the window the breaker is no longer tripped")
	}
	if !cb.RecordAndCheck("ns", "api") {
		t.Fatal("at exactly the window an action is allowed again")
	}
}

func TestCleanupIntervalIsHalfTheTTL(t *testing.T) {
	if got := cleanupInterval(10 * time.Minute); got != 5*time.Minute {
		t.Fatalf("cleanup every %s, want 5m", got)
	}
}

func TestActionLimiter_RemainingNeverNegative(t *testing.T) {
	c := newClock()
	l := NewActionLimiter(1, time.Minute)
	l.now = c.now
	if l.Remaining() != 1 || !l.Allow() || l.Remaining() != 0 || l.Allow() || l.Remaining() != 0 {
		t.Fatal("remaining counts down to zero and stays there")
	}
}

// D8: RecordAndCheck's own window check, with no IsTripped call before it
// (IsTripped clears an expired trip and would hide this boundary).
func TestCircuitBreaker_RecordAndCheckAtTheBoundary(t *testing.T) {
	c := newClock()
	cb := NewCircuitBreaker(2, time.Minute)
	cb.now = c.now
	cb.RecordAndCheck("ns", "api")
	if cb.RecordAndCheck("ns", "api") {
		t.Fatal("the second action trips the breaker")
	}
	c.advance(time.Minute)
	if !cb.RecordAndCheck("ns", "api") {
		t.Fatal("at exactly the window RecordAndCheck allows the action again")
	}
}
