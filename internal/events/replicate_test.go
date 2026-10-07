package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

type collect struct {
	mu  sync.Mutex
	got []string
}

func (c *collect) Record(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, e.Reason)
}

// ISS-059: the leader keeps every event and copies it to the standby; a
// standby keeps its own copy and sends nothing on.
func TestTee_CopiesOnlyWhileLeading(t *testing.T) {
	local, copied := &collect{}, &collect{}
	leading := true
	tee := Tee{Local: local, Copy: copied, Leading: func() bool { return leading }}
	tee.Record(Event{Reason: "as-leader"})
	leading = false
	tee.Record(Event{Reason: "as-standby"})
	if strings.Join(local.got, ",") != "as-leader,as-standby" || strings.Join(copied.got, ",") != "as-leader" {
		t.Fatalf("local %v, copied %v", local.got, copied.got)
	}
	Tee{Local: local}.Record(Event{Reason: "no-copy-configured"})
	if len(local.got) != 3 {
		t.Fatal("a tee without a copy still records locally")
	}
}

func TestReplicaForwarder_SendsToEveryStandbyMarkedRouted(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, name+" "+r.URL.Path+" routed="+r.Header.Get(RoutedHeader)+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}
	a, b := httptest.NewServer(handler("a")), httptest.NewServer(handler("b"))
	defer a.Close()
	defer b.Close()
	f := NewReplicaForwarder(func() ([]string, error) { return []string{a.URL, b.URL}, nil }, "t", nil)
	f.Record(Event{Reason: "x"})
	if n, err := f.Flush(context.Background()); err != nil || n != 1 || f.Pending() != 0 {
		t.Fatalf("flush: %d %v", n, err)
	}
	sort.Strings(seen)
	want := []string{"a " + IngestPath + " routed=1 Bearer t", "b " + IngestPath + " routed=1 Bearer t"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("seen %v", seen)
	}
}

func TestReplicaForwarder_NoStandbyAndLookupFailure(t *testing.T) {
	f := NewReplicaForwarder(func() ([]string, error) { return nil, nil }, "t", nil)
	f.Record(Event{})
	if n, err := f.Flush(context.Background()); err != nil || n != 1 || f.Pending() != 0 {
		t.Fatalf("with no standby the batch is done: %d %v pending %d", n, err, f.Pending())
	}
	g := NewReplicaForwarder(func() ([]string, error) { return nil, errors.New("api down") }, "t", nil)
	g.Record(Event{})
	if _, err := g.Flush(context.Background()); err == nil || g.Pending() != 1 {
		t.Fatal("a failed lookup keeps the events for the next try")
	}
}

// ISS-059: a controller that starts empty copies a peer's log, oldest
// first; one that already has events keeps its own.
func TestBackfill(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get(RoutedHeader) != "1" || r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode([]Event{{ID: 9, Reason: "newer"}, {ID: 8, Reason: "older"}}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer peer.Close()
	emptyPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer emptyPeer.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	peers := func() ([]string, error) { return []string{down.URL, emptyPeer.URL, peer.URL}, nil }

	rec := NewRecorder(10)
	n, err := Backfill(context.Background(), peers, "t", nil, rec)
	got := rec.Recent(10)
	if err != nil || n != 2 || len(got) != 2 || got[0].Reason != "newer" || got[1].Reason != "older" {
		t.Fatalf("backfill: n=%d err=%v got=%+v", n, err, got)
	}
	if n, _ := Backfill(context.Background(), peers, "t", nil, rec); n != 0 || rec.Count() != 2 {
		t.Fatal("a log with events is left alone")
	}
	if _, err := Backfill(context.Background(), func() ([]string, error) { return []string{down.URL}, nil }, "t", nil, NewRecorder(10)); err == nil {
		t.Fatal("no reachable peer is an error")
	}
	if _, err := Backfill(context.Background(), func() ([]string, error) { return nil, errors.New("api down") }, "t", nil, NewRecorder(10)); err == nil {
		t.Fatal("a failed lookup is an error")
	}
}
