package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// SlackAction represents a button click from Slack interactive message.
type SlackAction struct {
	ActionID string
	Value    string
	UserID   string
	UserName string
}

// SlackActionHandler processes incoming Slack interactive message callbacks.
// Register this at /api/slack/actions in the mux.
type SlackActionHandler struct {
	signingSecret string
	onApprove     func(incidentID string) string
	onRollback    func(incidentID string) string
	onSilence     func(nsWorkload string, duration time.Duration) string
}

func NewSlackActionHandler(signingSecret string) *SlackActionHandler {
	return &SlackActionHandler{signingSecret: signingSecret}
}

func (h *SlackActionHandler) SetCallbacks(
	onApprove func(string) string,
	onRollback func(string) string,
	onSilence func(string, time.Duration) string,
) {
	h.onApprove = onApprove
	h.onRollback = onRollback
	h.onSilence = onSilence
}

func (h *SlackActionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	// Slack sends payload as form-encoded: payload=JSON
	values, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "parse", http.StatusBadRequest)
		return
	}

	payloadStr := values.Get("payload")
	if payloadStr == "" {
		http.Error(w, "no payload", http.StatusBadRequest)
		return
	}

	var payload struct {
		Type    string `json:"type"`
		User    struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		http.Error(w, "parse payload", http.StatusBadRequest)
		return
	}

	if payload.Type != "block_actions" {
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, action := range payload.Actions {
		klog.Infof("slack: action %s value=%s by user=%s", action.ActionID, action.Value, payload.User.Username)

		var responseText string
		switch action.ActionID {
		case "approve_fix":
			if h.onApprove != nil {
				responseText = h.onApprove(action.Value)
			} else {
				responseText = fmt.Sprintf("Approved by %s (handler not configured)", payload.User.Username)
			}
		case "rollback":
			if h.onRollback != nil {
				responseText = h.onRollback(action.Value)
			} else {
				responseText = fmt.Sprintf("Rollback requested by %s (handler not configured)", payload.User.Username)
			}
		case "silence_1h":
			if h.onSilence != nil {
				responseText = h.onSilence(action.Value, 1*time.Hour)
			} else {
				responseText = fmt.Sprintf("Silenced for 1h by %s", payload.User.Username)
			}
		default:
			responseText = fmt.Sprintf("Unknown action: %s", action.ActionID)
		}

		// Respond to Slack with updated message
		resp := map[string]interface{}{
			"response_type":    "in_channel",
			"replace_original": true,
			"text":             responseText,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// RegisterSlackActions adds the /api/slack/actions endpoint to the server.
func RegisterSlackActions(mux *http.ServeMux, handler *SlackActionHandler) {
	mux.Handle("/api/slack/actions", handler)
}

// parseNsWorkload splits "namespace/workload" from silence button value.
func parseNsWorkload(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return s, ""
}
