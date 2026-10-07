package events

import (
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
