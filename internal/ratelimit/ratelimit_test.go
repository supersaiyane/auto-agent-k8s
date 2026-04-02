package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestDeduplicator_BasicDedup(t *testing.T) {
	d := NewDeduplicator(100 * time.Millisecond)
	defer d.Stop()

	// First check should pass
	if !d.Check("key1") {
		t.Fatal("expected first check to return true")
	}

	// Second check within TTL should be deduplicated
	if d.Check("key1") {
		t.Fatal("expected second check within TTL to return false")
	}

	// Different key should pass
	if !d.Check("key2") {
		t.Fatal("expected different key to return true")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Should pass again after TTL
	if !d.Check("key1") {
		t.Fatal("expected check after TTL expiry to return true")
	}
}

func TestDeduplicator_Reset(t *testing.T) {
	d := NewDeduplicator(5 * time.Second)
	defer d.Stop()

	d.Check("key1")
	if d.Check("key1") {
		t.Fatal("expected dedup within TTL")
	}

	d.Reset("key1")
	if !d.Check("key1") {
		t.Fatal("expected check after reset to return true")
	}
}

func TestDeduplicator_Concurrent(t *testing.T) {
	d := NewDeduplicator(1 * time.Second)
	defer d.Stop()

	var wg sync.WaitGroup
	passed := int32(0)
	var mu sync.Mutex

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Check("concurrent-key") {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if passed != 1 {
		t.Fatalf("expected exactly 1 pass through dedup, got %d", passed)
	}
}

func TestActionLimiter_Basic(t *testing.T) {
	l := NewActionLimiter(3, 1*time.Second)

	// Should allow 3 actions
	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("expected Allow() to return true on call %d", i+1)
		}
	}

	// 4th should be blocked
	if l.Allow() {
		t.Fatal("expected Allow() to return false after limit reached")
	}

	if l.Remaining() != 0 {
		t.Fatalf("expected 0 remaining, got %d", l.Remaining())
	}
}

func TestActionLimiter_WindowExpiry(t *testing.T) {
	l := NewActionLimiter(2, 100*time.Millisecond)

	l.Allow()
	l.Allow()

	if l.Allow() {
		t.Fatal("expected limit reached")
	}

	// Wait for window to expire
	time.Sleep(150 * time.Millisecond)

	if !l.Allow() {
		t.Fatal("expected Allow() after window expiry")
	}
}

func TestActionLimiter_Count(t *testing.T) {
	l := NewActionLimiter(10, 1*time.Second)

	l.Allow()
	l.Allow()
	l.Allow()

	if l.Count() != 3 {
		t.Fatalf("expected count 3, got %d", l.Count())
	}

	if l.Remaining() != 7 {
		t.Fatalf("expected 7 remaining, got %d", l.Remaining())
	}
}

func TestActionLimiter_Concurrent(t *testing.T) {
	l := NewActionLimiter(5, 1*time.Second)
	var wg sync.WaitGroup
	allowed := int32(0)
	var mu sync.Mutex

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != 5 {
		t.Fatalf("expected exactly 5 allowed, got %d", allowed)
	}
}
