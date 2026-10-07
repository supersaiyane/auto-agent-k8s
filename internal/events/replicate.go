package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx"
)

// RoutedHeader marks a request that must be served by the pod it reached:
// a standby forwarding to the leader sets it, and so does the leader when it
// copies events to the standby, so the copy is stored, not proxied back.
const RoutedHeader = "X-Auto-Agent-Proxied"

// Tee records every event locally and, while this controller leads, copies
// it to the standby controllers, so a leader change keeps the history
// (ISS-059, ADR-001).
type Tee struct {
	Local   Sink
	Copy    Sink
	Leading func() bool
}

// Record implements Sink.
func (t Tee) Record(e Event) {
	t.Local.Record(e)
	if t.Copy != nil && t.Leading != nil && t.Leading() {
		t.Copy.Record(e)
	}
}

// NewReplicaForwarder sends batches to every standby controller that peers
// returns, marked with RoutedHeader. With no standby the batch is dropped:
// there is no one to keep a copy.
func NewReplicaForwarder(peers func() ([]string, error), token string, hc *http.Client) *Forwarder {
	return &Forwarder{peers: peers, token: token, routed: true,
		client: httpx.Client(hc, 10*time.Second), max: forwardBuffer}
}

// Backfill copies a peer controller's event log into an empty local log,
// oldest first, so a controller that just started holds the history before
// it can become leader (ISS-059). A log that already has events is left
// alone, so a restart never doubles them. It returns how many it imported.
func Backfill(ctx context.Context, peers func() ([]string, error), token string, hc *http.Client, into *Recorder) (int, error) {
	if into.Count() > 0 {
		return 0, nil
	}
	bases, err := peers()
	if err != nil {
		return 0, fmt.Errorf("backfill: peers: %w", err)
	}
	client := httpx.Client(hc, 10*time.Second)
	var lastErr error
	for _, b := range bases {
		evts, err := fetchLog(ctx, client, b+IngestPath, token)
		if err != nil {
			lastErr = err
			continue
		}
		if len(evts) == 0 {
			continue
		}
		for i := len(evts) - 1; i >= 0; i-- { // the peer answers newest first
			e := evts[i]
			e.ID = 0
			into.Record(e)
		}
		return len(evts), nil
	}
	return 0, lastErr
}

func fetchLog(ctx context.Context, client *http.Client, url, token string) ([]Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("backfill: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(RoutedHeader, "1") // the peer's own log, not the leader's
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backfill: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backfill: %s answered %d", url, resp.StatusCode)
	}
	var evts []Event
	if err := json.NewDecoder(resp.Body).Decode(&evts); err != nil {
		return nil, fmt.Errorf("backfill: decode: %w", err)
	}
	return evts, nil
}
