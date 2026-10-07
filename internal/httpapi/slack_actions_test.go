package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSigningSecret = "8f742231b10e8888abcd99yyyzzz85a5"

func slackBody() string {
	payload := `{"type":"block_actions","user":{"id":"U1","username":"alice"},"actions":[{"action_id":"approve_fix","value":"inc-1"}]}`
	return "payload=" + url.QueryEscape(payload)
}

func sign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

func postSlack(h http.Handler, body, ts, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, slackActionsPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ISS-006: Slack callbacks must carry a valid, fresh Slack signature.
func TestSlackActions_Signature(t *testing.T) {
	h := NewSlackActionHandler(testSigningSecret)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	body := slackBody()

	for _, tc := range []struct {
		name     string
		body, ts string
		sig      string
		want     int
	}{
		{"valid", body, now, sign(testSigningSecret, now, body), http.StatusOK},
		{"no signature", body, now, "", http.StatusUnauthorized},
		{"wrong secret", body, now, sign("other", now, body), http.StatusUnauthorized},
		{"tampered body", body + "x", now, sign(testSigningSecret, now, body), http.StatusUnauthorized},
		{"replayed (stale timestamp)", body, stale, sign(testSigningSecret, stale, body), http.StatusUnauthorized},
		{"bad timestamp", body, "soon", sign(testSigningSecret, "soon", body), http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := postSlack(h, tc.body, tc.ts, tc.sig).Code; got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSlackActions_NoSecretFailsClosed(t *testing.T) {
	h := NewSlackActionHandler("")
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if got := postSlack(h, slackBody(), now, sign("", now, slackBody())).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 without SLACK_SIGNING_SECRET", got)
	}
}

// With no callback wired, the reply must not claim the action happened.
func TestSlackActions_UnwiredActionSaysNothingHappened(t *testing.T) {
	h := NewSlackActionHandler(testSigningSecret)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	rec := postSlack(h, slackBody(), now, sign(testSigningSecret, now, slackBody()))
	if !strings.Contains(rec.Body.String(), "no action was taken") {
		t.Fatalf("reply must say no action was taken, got %s", rec.Body.String())
	}
}
