package events

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

var fixedNow = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

func accept(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func decode(t *testing.T, body string) []Event {
	t.Helper()
	var out []Event
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestForwarder_SendsBatchesWithTokenAndNode(t *testing.T) {
	var auth []string
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		accept(w, r)
	})
	defer s.Close()
	f := NewForwarder(s.URL, "internal-secret", "node-a", s.Client(time.Second))
	f.now = func() time.Time { return fixedNow }
	f.Record(Event{Reason: "CrashLoopBackOff", Namespace: "default"})
	f.Record(Event{Reason: "OOMKilled", Node: "node-b", Timestamp: fixedNow.Add(-time.Minute)})
	if n, err := f.Flush(context.Background()); err != nil || n != 2 || f.Pending() != 0 {
		t.Fatalf("flush: n=%d err=%v pending=%d", n, err, f.Pending())
	}
	r := s.Requests()
	if len(r) != 1 || r[0].Method != "POST" || r[0].Path != IngestPath || auth[0] != "Bearer internal-secret" {
		t.Fatalf("request: %+v auth %v", r, auth)
	}
	got := decode(t, r[0].Body)
	if got[0].Node != "node-a" || !got[0].Timestamp.Equal(fixedNow) || got[1].Node != "node-b" || !got[1].Timestamp.Equal(fixedNow.Add(-time.Minute)) {
		t.Fatalf("stamping: %+v", got)
	}
	if n, err := f.Flush(context.Background()); n != 0 || err != nil || len(s.Requests()) != 1 {
		t.Fatal("an empty queue sends nothing")
	}
}

func TestForwarder_FailureKeepsEvents(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"refused": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusUnauthorized) },
		"timeout": httpxtest.Slow(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			s := httpxtest.New(h)
			defer s.Close()
			f := NewForwarder(s.URL, "t", "n", s.Client(100*time.Millisecond))
			f.Record(Event{Reason: "x"})
			if _, err := f.Flush(context.Background()); err == nil || f.Pending() != 1 {
				t.Fatalf("a failed send keeps the event: pending=%d err=%v", f.Pending(), err)
			}
		})
	}
	f := NewForwarder("http://[::1", "t", "n", nil) // malformed URL
	f.Record(Event{})
	if _, err := f.Flush(context.Background()); err == nil {
		t.Fatal("a bad URL is an error")
	}
}

// A full buffer drops the oldest events and counts them; a drop while a
// request is in flight removes only what was actually sent.
func TestForwarder_BoundedBufferAndInFlightDrops(t *testing.T) {
	var f *Forwarder
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ { // overflow while the batch is in flight
			f.Record(Event{Reason: "late"})
		}
		accept(w, r)
	})
	defer s.Close()
	f = NewForwarder(s.URL, "t", "n", s.Client(time.Second))
	f.max = 3
	before := testutil.ToFloat64(obs.EventsDroppedTotal)
	for _, r := range []string{"a", "b", "c", "d"} {
		f.Record(Event{Reason: r})
	}
	if f.Pending() != 3 || testutil.ToFloat64(obs.EventsDroppedTotal)-before != 1 {
		t.Fatalf("pending %d, dropped %v", f.Pending(), testutil.ToFloat64(obs.EventsDroppedTotal)-before)
	}
	if n, err := f.Flush(context.Background()); err != nil || n != 3 {
		t.Fatalf("flush: %d %v", n, err)
	}
	sent := decode(t, s.Requests()[0].Body)
	if sent[0].Reason != "b" || sent[2].Reason != "d" {
		t.Fatalf("oldest dropped first: %+v", sent)
	}
	if f.Pending() != 3 {
		t.Fatalf("the three late events stay queued, got %d", f.Pending())
	}
	for _, q := range f.buf {
		if q.ev.Reason != "late" {
			t.Fatalf("only unsent events remain: %+v", f.buf)
		}
	}
}

// Run sends on its interval, in full batches, and drains once more on stop.
func TestForwarder_RunDrainsOnStop(t *testing.T) {
	s := httpxtest.New(accept)
	defer s.Close()
	f := NewForwarder(s.URL, "t", "n", s.Client(time.Second))
	for i := 0; i < MaxBatch+5; i++ {
		f.Record(Event{Reason: "r"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx, time.Hour); close(done) }()
	cancel()
	<-done
	if f.Pending() != 0 || len(s.Requests()) != 2 {
		t.Fatalf("drained in two batches on stop: pending %d, requests %d", f.Pending(), len(s.Requests()))
	}

	down := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 503) })
	defer down.Close()
	g := NewForwarder(down.URL, "t", "n", down.Client(time.Second))
	g.Record(Event{})
	errs := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("forwarder", "post"))
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	g.Run(ctx, 10*time.Millisecond)
	if g.Pending() != 1 || testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("forwarder", "post")) <= errs {
		t.Fatal("failed sends are counted and the event kept")
	}
	if !strings.HasSuffix(g.url, IngestPath) {
		t.Fatal("posts to the ingest path")
	}
}

// *Recorder and *Forwarder are both sinks.
var _ = []Sink{&Recorder{}, &Forwarder{}}
