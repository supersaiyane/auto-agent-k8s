package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// CircuitBreaker tracks actions per workload and trips when a threshold
// is exceeded within a window. Once tripped, all actions for that workload
// are blocked until the window expires.
type CircuitBreaker struct {
	mu        sync.Mutex
	actions   map[string][]time.Time // key: "ns/workload" -> timestamps
	threshold int
	window    time.Duration
	tripped   map[string]time.Time // key -> when it tripped
}

func NewCircuitBreaker(threshold int, window time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if window <= 0 {
		window = 1 * time.Hour
	}
	return &CircuitBreaker{
		actions:   make(map[string][]time.Time),
		threshold: threshold,
		window:    window,
		tripped:   make(map[string]time.Time),
	}
}

// RecordAndCheck records an action for the workload and returns true if allowed.
// Returns false if the circuit breaker has tripped for this workload.
func (cb *CircuitBreaker) RecordAndCheck(ns, workload string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	key := fmt.Sprintf("%s/%s", ns, workload)
	now := time.Now()

	// Check if already tripped
	if tripTime, ok := cb.tripped[key]; ok {
		if now.Sub(tripTime) < cb.window {
			return false // still tripped
		}
		// Window expired, reset
		delete(cb.tripped, key)
		delete(cb.actions, key)
	}

	// Prune old actions outside window
	cutoff := now.Add(-cb.window)
	existing := cb.actions[key]
	pruned := existing[:0]
	for _, t := range existing {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}

	// Record this action
	pruned = append(pruned, now)
	cb.actions[key] = pruned

	// Check threshold
	if len(pruned) >= cb.threshold {
		cb.tripped[key] = now
		return false // just tripped
	}
	return true
}

// IsTripped returns true if the circuit breaker is currently tripped for this workload.
func (cb *CircuitBreaker) IsTripped(ns, workload string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	key := fmt.Sprintf("%s/%s", ns, workload)
	tripTime, ok := cb.tripped[key]
	if !ok {
		return false
	}
	if time.Since(tripTime) >= cb.window {
		delete(cb.tripped, key)
		delete(cb.actions, key)
		return false
	}
	return true
}

// TrippedWorkloads returns all currently tripped workload keys.
func (cb *CircuitBreaker) TrippedWorkloads() []string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()
	var result []string
	for key, tripTime := range cb.tripped {
		if now.Sub(tripTime) < cb.window {
			result = append(result, key)
		}
	}
	return result
}
