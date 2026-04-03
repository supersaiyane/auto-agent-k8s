package events

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/klog/v2"
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

// Recorder is a thread-safe in-memory ring buffer backed by a JSONL file on disk.
// Events survive pod restarts when written to a persistent volume.
type Recorder struct {
	mu       sync.RWMutex
	events   []Event
	maxLen   int
	nextID   int
	filePath string
	file     *os.File
	encoder  *json.Encoder
}

// NewRecorder creates a recorder. If persistPath is non-empty, events are
// written to disk and reloaded on startup.
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

// EnablePersistence enables writing events to a JSONL file and loads existing events.
func (r *Recorder) EnablePersistence(path string) {
	if path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.filePath = path

	// Load existing events from file
	r.loadFromFile()

	// Open file for appending
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		klog.Warningf("events: cannot create dir for %s: %v", path, err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		klog.Warningf("events: cannot open %s: %v", path, err)
		return
	}
	r.file = f
	r.encoder = json.NewEncoder(f)
	klog.Infof("events: persistence enabled at %s (%d events loaded)", path, len(r.events))
}

func (r *Recorder) loadFromFile() {
	f, err := os.Open(r.filePath)
	if err != nil {
		return // file doesn't exist yet
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	count := 0
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.ID >= r.nextID {
			r.nextID = e.ID + 1
		}
		if len(r.events) >= r.maxLen {
			r.events = r.events[1:]
		}
		r.events = append(r.events, e)
		count++
	}
	if count > 0 {
		klog.Infof("events: loaded %d events from %s", count, r.filePath)
	}
}

// Record adds an event. If persistence is enabled, also writes to disk.
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.ID = r.nextID
	r.nextID++
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if len(r.events) >= r.maxLen {
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = e
	} else {
		r.events = append(r.events, e)
	}

	// Persist to file
	if r.encoder != nil {
		if err := r.encoder.Encode(e); err != nil {
			klog.V(3).Infof("events: write failed: %v", err)
		}
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
			break
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

// Close closes the persistence file.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		r.file.Close()
		r.file = nil
		r.encoder = nil
	}
}
