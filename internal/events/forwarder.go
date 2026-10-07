package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// Sink receives recorded events. A *Recorder stores them; a *Forwarder
// sends them to the controller, which owns the one event log (ADR-001).
type Sink interface {
	Record(Event)
}

const (
	// IngestPath is where the controller accepts forwarded events.
	IngestPath = "/internal/v1/events"
	// MaxBatch is the most events one forward request carries.
	MaxBatch = 200
	// forwardBuffer bounds the events a node agent holds while the
	// controller is unreachable; past it the oldest are dropped and counted.
	forwardBuffer = 2000
)

type queued struct {
	seq uint64
	ev  Event
}

// Forwarder buffers events on a node agent and posts them in batches to
// the controller. Record never blocks a handler.
type Forwarder struct {
	url, token, node string
	// peers, when set, replaces url: the batch goes to every address it
	// returns (controller replication, ISS-059). routed marks those requests
	// so the receiving controller stores them instead of proxying them.
	peers  func() ([]string, error)
	routed bool
	client *http.Client
	now    func() time.Time // injectable clock; nil means time.Now
	max    int

	mu      sync.Mutex
	buf     []queued
	nextSeq uint64
}

// NewForwarder posts to baseURL+IngestPath with the internal token, and
// stamps events that carry no node with this agent's node.
func NewForwarder(baseURL, token, node string, hc *http.Client) *Forwarder {
	return &Forwarder{url: baseURL + IngestPath, token: token, node: node,
		client: httpx.Client(hc, 10*time.Second), max: forwardBuffer}
}

func (f *Forwarder) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// Record queues an event. A full buffer drops the oldest event.
func (f *Forwarder) Record(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = f.clock().UTC()
	}
	if e.Node == "" {
		e.Node = f.node
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buf) >= f.max {
		f.buf = f.buf[1:]
		obs.EventsDroppedTotal.Inc()
	}
	f.nextSeq++
	f.buf = append(f.buf, queued{seq: f.nextSeq, ev: e})
}

// Pending is the number of events not yet accepted by the controller.
func (f *Forwarder) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buf)
}

// Flush sends one batch. On failure the events stay queued for the next try.
// It returns how many events the controller accepted.
func (f *Forwarder) Flush(ctx context.Context) (int, error) {
	f.mu.Lock()
	n := min(len(f.buf), MaxBatch)
	batch := make([]Event, n)
	var last uint64
	for i := 0; i < n; i++ {
		batch[i] = f.buf[i].ev
		last = f.buf[i].seq
	}
	f.mu.Unlock()
	if n == 0 {
		return 0, nil
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, fmt.Errorf("forwarder: encode: %w", err)
	}
	targets := []string{f.url}
	if f.peers != nil {
		bases, err := f.peers()
		if err != nil {
			return 0, fmt.Errorf("forwarder: peers: %w", err)
		}
		targets = targets[:0]
		for _, b := range bases {
			targets = append(targets, b+IngestPath)
		}
	}
	for _, u := range targets { // none: no standby to keep a copy, so the batch is done
		if err := f.post(ctx, u, body); err != nil {
			return 0, err
		}
	}
	// Remove what was sent by sequence number: events dropped while the
	// request was in flight must not shift the window onto unsent ones.
	f.mu.Lock()
	i := 0
	for i < len(f.buf) && f.buf[i].seq <= last {
		i++
	}
	f.buf = f.buf[i:]
	f.mu.Unlock()
	return n, nil
}

func (f *Forwarder) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("forwarder: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	if f.routed {
		req.Header.Set(RoutedHeader, "1")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("forwarder: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("forwarder: %s answered %d", url, resp.StatusCode)
	}
	return nil
}

// Run flushes every interval until ctx ends, then tries once more briefly
// so events recorded during shutdown are not lost.
func (f *Forwarder) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			f.drain(final)
			cancel()
			return
		case <-t.C:
			f.drain(ctx)
		}
	}
}

// drain sends full batches until the queue is empty or a send fails.
func (f *Forwarder) drain(ctx context.Context) {
	for {
		n, err := f.Flush(ctx)
		if err != nil {
			obs.HandlerErrorsTotal.WithLabelValues("forwarder", "post").Inc()
			klog.V(2).Infof("%v (%d events queued)", err, f.Pending())
			return
		}
		if n < MaxBatch {
			return
		}
	}
}
