// Package httpx holds the one rule every outbound client follows (PLAN-002
// 9.1, 9.2): use the injected *http.Client when there is one, otherwise a
// client with the caller's timeout. Tests inject a client pointed at
// httptest; production passes nil.
package httpx

import (
	"net/http"
	"time"
)

// Client returns hc, or a new client with the given timeout when hc is nil.
func Client(hc *http.Client, timeout time.Duration) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: timeout}
}
