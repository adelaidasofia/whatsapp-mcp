package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// NotifyRequest is the public surface for /api/notify-owner.
type NotifyRequest struct {
	Message             string `json:"message"`
	Urgency             string `json:"urgency"`
	DeeplinkToInboxFile string `json:"deeplinkToInboxFile,omitempty"`
}

// NotifyResponse is the response for /api/notify-owner.
type NotifyResponse struct {
	Delivered   bool   `json:"delivered"`
	DeliveredAt string `json:"deliveredAt,omitempty"` // ISO8601
	LatencyMs   int64  `json:"latencyMs"`
}

// handleNotifyOwner serves POST /api/notify-owner.
//
// Checks quiet hours and the shared WhatsApp ping budget (10/hr) before
// sending. When either blocks the send, the message is queued in
// pending_pings and delivered: false is returned — no error, the caller
// should treat it as gracefully queued.
func (s *Server) handleNotifyOwner(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req NotifyRequest
	if err := readJSONBody(r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "message is required", false)
		return
	}
	if len(req.Message) > 500 {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "message must be ≤500 characters", false)
		return
	}
	if req.Urgency != "low" && req.Urgency != "normal" && req.Urgency != "high" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "urgency must be low, normal, or high", false)
		return
	}

	// Quiet hours AND budget intentionally DISABLED for /api/notify-owner
	// (codified 2026-05-03 by the owner: "I don't want quiet hours, send right
	// away always" + "always the best of the best, never put off for later
	// what we can do now"). The concierge real-time activity stream is the
	// primary delivery surface for this endpoint — every guest action lands
	// in WhatsApp as it happens, no rate-limit, no overnight queueing.
	// Other prospect-api endpoints that ping the owner (digest scheduler,
	// etc.) keep their own quiet-hours and budget behavior — this exemption
	// is scoped to /api/notify-owner only. Budget tracking is still
	// recorded for observability (so we can see traffic shape over time)
	// but the response no longer gates on it.
	_ = start // keep `start` reachable for latency calc below

	budget := &waPingBudget{db: s.presetDB, limitPH: s.cfg.WhatsAppPingsPerHour}
	if _, err := budget.CheckAndRecordPing("/api/notify-owner", req.Urgency); err != nil {
		log.Printf("notify-owner: budget record (non-blocking): %v", err)
	}

	deliveredAt, err := s.sendViaBridge(req.Message)
	if err != nil {
		log.Printf("notify-owner: bridge send: %v", err)
		writeError(w, http.StatusBadGateway, ErrInternal, "bridge send failed: "+err.Error(), true)
		return
	}

	resp := NotifyResponse{
		Delivered:   true,
		DeliveredAt: deliveredAt,
		LatencyMs:   time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/notify-owner",
		params:     map[string]any{"urgency": req.Urgency, "msgLen": len(req.Message)},
		result:     "delivered",
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

// sendViaBridge delivers text to the user's own WhatsApp JID via the bridge
// two-step flow: POST /api/sends → POST /api/sends/{id}/confirm.
// Shared by notify-owner and relay-note.
func (s *Server) sendViaBridge(text string) (string, error) {
	selfJID, err := s.resolveSelfJID()
	if err != nil {
		return "", fmt.Errorf("resolve self JID: %w", err)
	}

	// WhatsApp bridge /api/sends draft schema (whatsapp-bridge/sends.go
	// `createDraftRequest`): requires `send_type` ("text" | "reply_quote"
	// | "reaction"), `recipient_jid`, and `text` for text sends. The
	// prospect-api v1 schema was off in three field names — fixed 2026-05-03
	// after the queue stuck on successive 400s ("jid" → "recipient_jid",
	// "message" → "text", missing "send_type": "text").
	draftPayload, _ := json.Marshal(map[string]string{
		"send_type":     "text",
		"recipient_jid": selfJID,
		"text":          text,
	})
	draftBody, err := s.bridgeRequest("POST", s.cfg.BridgeBaseURL+"/api/sends", draftPayload)
	if err != nil {
		return "", fmt.Errorf("create draft: %w", err)
	}

	var draft struct {
		DraftID string `json:"draft_id"`
	}
	if err := json.Unmarshal(draftBody, &draft); err != nil || draft.DraftID == "" {
		return "", fmt.Errorf("draft parse: jid missing (body: %s)", string(draftBody))
	}

	confirmURL := fmt.Sprintf("%s/api/sends/%s/confirm", s.cfg.BridgeBaseURL, draft.DraftID)
	if _, err := s.bridgeRequest("POST", confirmURL, nil); err != nil {
		return "", fmt.Errorf("confirm send: %w", err)
	}
	return time.Now().UTC().Format(time.RFC3339), nil
}

// resolveSelfJID returns the user's own WhatsApp JID. Reads PROSPECT_SELF_JID
// from environment first; falls back to /api/status on the bridge.
//
// Bridge /api/status returns the full device JID under `device_jid`, e.g.
// "12145180889:58@s.whatsapp.net". The trailing `:NN` is a device-specific
// resource ID that breaks WhatsApp's "message yourself" feature — strip it
// to the bare JID before returning. Codified 2026-05-03 after the notify
// pipeline shipped and queued pings were stuck on the wrong JSON field
// name (was `jid`, actual field is `device_jid`).
func (s *Server) resolveSelfJID() (string, error) {
	if jid := os.Getenv("PROSPECT_SELF_JID"); jid != "" {
		return jid, nil
	}
	statusBody, err := s.bridgeRequest("GET", s.cfg.BridgeBaseURL+"/api/status", nil)
	if err != nil {
		return "", fmt.Errorf("bridge status: %w", err)
	}
	var status struct {
		DeviceJID string `json:"device_jid"`
	}
	if err := json.Unmarshal(statusBody, &status); err != nil || status.DeviceJID == "" {
		return "", fmt.Errorf("bridge status device_jid missing (body: %s)", string(statusBody))
	}
	// Strip device-resource suffix (the part after ":") to get the bare
	// JID that "message yourself" expects.
	jid := status.DeviceJID
	if at := strings.Index(jid, "@"); at != -1 {
		if colon := strings.Index(jid[:at], ":"); colon != -1 {
			jid = jid[:colon] + jid[at:]
		}
	}
	return jid, nil
}

// bridgeRequest sends an authenticated HTTP request to the whatsapp-bridge and
// returns the body on 2xx. Non-2xx is returned as an error.
func (s *Server) bridgeRequest(method, url string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.BridgeAuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bridge returned %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
