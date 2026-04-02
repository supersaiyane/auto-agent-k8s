package ratelimit

import (
	"sync"
	"time"
)

// Deduplicator prevents the same event from being processed multiple times
// within a TTL window. Key format: "namespace/pod/reason".
type Deduplicator struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	done chan struct{}
}

func NewDeduplicator(ttl time.Duration) *Deduplicator {
	d := &Deduplicator{
		seen: make(map[string]time.Time),
		ttl:  ttl,
		done: make(chan struct{}),
	}
	go d.cleanup()
	return d
}

// Check returns true if this key should be processed (not seen within TTL).
func (d *Deduplicator) Check(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[key]; ok && time.Since(t) < d.ttl {
		return false
	}
	d.seen[key] = time.Now()
	return true
}

// Reset removes a key so it can be processed again immediately.
func (d *Deduplicator) Reset(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
}

func (d *Deduplicator) Stop() {
	close(d.done)
}

func (d *Deduplicator) cleanup() {
	ticker := time.NewTicker(d.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.mu.Lock()
			now := time.Now()
			for k, t := range d.seen {
				if now.Sub(t) >= d.ttl {
					delete(d.seen, k)
				}
			}
			d.mu.Unlock()
		}
	}
}

// ActionLimiter enforces a maximum number of actions within a sliding window.
type ActionLimiter struct {
	mu     sync.Mutex
	events []time.Time
	max    int
	window time.Duration
}

func NewActionLimiter(max int, window time.Duration) *ActionLimiter {
	return &ActionLimiter{
		events: make([]time.Time, 0, max),
		max:    max,
		window: window,
	}
}

// Allow returns true if an action is permitted (under the rate limit).
// It records the action if allowed.
func (l *ActionLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	if len(l.events) >= l.max {
		return false
	}
	l.events = append(l.events, time.Now())
	return true
}

// Count returns the current number of actions in the window.
func (l *ActionLimiter) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	return len(l.events)
}

// Remaining returns how many more actions are allowed in the current window.
func (l *ActionLimiter) Remaining() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	r := l.max - len(l.events)
	if r < 0 {
		return 0
	}
	return r
}

func (l *ActionLimiter) prune() {
	cutoff := time.Now().Add(-l.window)
	i := 0
	for i < len(l.events) && l.events[i].Before(cutoff) {
		i++
	}
	l.events = l.events[i:]
}
