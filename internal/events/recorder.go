package events

import (
	"sync"
	"time"
)

// EventType categorizes recorded events.
type EventType string

const (
	Incident EventType = "incident"
	Action   EventType = "action"
	Scaling  EventType = "scaling"
	Anomaly  EventType = "anomaly"
	Info     EventType = "info"
)

// Severity levels for display.
type Severity string

const (
	SevCritical Severity = "critical"
	SevWarning  Severity = "warning"
	SevInfo     Severity = "info"
)

// Event is a single recorded event for the dashboard.
type Event struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      EventType `json:"type"`
	Severity  Severity  `json:"severity"`
	Namespace string    `json:"namespace"`
	Workload  string    `json:"workload"`
	Pod       string    `json:"pod,omitempty"`
	Node      string    `json:"node,omitempty"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Action    string    `json:"action,omitempty"`
	LogURL    string    `json:"logUrl,omitempty"`
	PRURL     string    `json:"prUrl,omitempty"`
	TicketURL string    `json:"ticketUrl,omitempty"`
}

// Recorder is a thread-safe in-memory ring buffer for events.
type Recorder struct {
	mu     sync.RWMutex
	events []Event
	maxLen int
	nextID int
}

// NewRecorder creates a recorder with the given max capacity.
func NewRecorder(maxLen int) *Recorder {
	if maxLen <= 0 {
		maxLen = 500
	}
	return &Recorder{
		events: make([]Event, 0, maxLen),
		maxLen: maxLen,
		nextID: 1,
	}
}

// Record adds an event. If the buffer is full, the oldest event is dropped.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.ID = r.nextID
	r.nextID++
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if len(r.events) >= r.maxLen {
		// Shift left: drop oldest
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = e
	} else {
		r.events = append(r.events, e)
	}
}

// Recent returns the last N events in reverse chronological order (newest first).
func (r *Recorder) Recent(n int) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.events)
	if n <= 0 || n > total {
		n = total
	}
	result := make([]Event, n)
	for i := 0; i < n; i++ {
		result[i] = r.events[total-1-i]
	}
	return result
}

// Since returns events after the given timestamp, newest first.
func (r *Recorder) Since(t time.Time) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []Event
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Timestamp.After(t) {
			result = append(result, r.events[i])
		} else {
			break // events are ordered, so we can stop early
		}
	}
	return result
}

// Count returns the total number of stored events.
func (r *Recorder) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.events)
}

// Stats returns counts by type.
func (r *Recorder) Stats() map[EventType]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := map[EventType]int{}
	for _, e := range r.events {
		m[e.Type]++
	}
	return m
}
