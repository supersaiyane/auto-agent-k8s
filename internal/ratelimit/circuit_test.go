package ratelimit

import (
	"testing"
	"time"
)

func TestCircuitBreaker_Basic(t *testing.T) {
	cb := NewCircuitBreaker(3, 1*time.Hour)

	// First 2 actions: allowed
	if !cb.RecordAndCheck("default", "api") {
		t.Fatal("expected first action allowed")
	}
	if !cb.RecordAndCheck("default", "api") {
		t.Fatal("expected second action allowed")
	}

	// 3rd action: trips the breaker
	if cb.RecordAndCheck("default", "api") {
		t.Fatal("expected third action to trip breaker")
	}

	// Subsequent actions: blocked
	if cb.RecordAndCheck("default", "api") {
		t.Fatal("expected action blocked after trip")
	}

	if !cb.IsTripped("default", "api") {
		t.Fatal("expected IsTripped=true")
	}

	// Different workload: not affected
	if !cb.RecordAndCheck("default", "web") {
		t.Fatal("expected different workload to be allowed")
	}
}

func TestCircuitBreaker_WindowExpiry(t *testing.T) {
	cb := NewCircuitBreaker(2, 100*time.Millisecond)

	cb.RecordAndCheck("default", "api")
	cb.RecordAndCheck("default", "api") // trips

	if cb.RecordAndCheck("default", "api") {
		t.Fatal("expected blocked")
	}

	time.Sleep(150 * time.Millisecond)

	// Window expired, should allow again
	if !cb.RecordAndCheck("default", "api") {
		t.Fatal("expected allowed after window expiry")
	}
}

func TestCircuitBreaker_TrippedWorkloads(t *testing.T) {
	cb := NewCircuitBreaker(1, 1*time.Hour)

	cb.RecordAndCheck("default", "api")   // trips
	cb.RecordAndCheck("prod", "worker")   // trips

	tripped := cb.TrippedWorkloads()
	if len(tripped) != 2 {
		t.Fatalf("expected 2 tripped, got %d", len(tripped))
	}
}
