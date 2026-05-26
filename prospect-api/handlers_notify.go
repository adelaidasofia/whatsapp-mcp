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

	qh, err := newQuietHoursChecker(s.cfg)
	if err != nil {
		log.Printf("notify-owner: quiet hours init: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "quiet hours config error: "+err.Error(), true)
		return
	}

	now := time.Now()
	if !qh.ShouldSendNow(req.Urgency, now) {
		deliverAt := qh.NextWakeTime(now)
		if err := EnqueuePing(s.presetDB, req.Message, req.Urgency, req.DeeplinkToInboxFile, deliverAt); err != nil {
			log.Printf("notify-owner: enqueue: %v", err)
			writeError(w, http.StatusInternalServerError, ErrInternal, "enqueue failed: "+err.Error(), true)
			return
		}
		resp := NotifyResponse{Delivered: false, LatencyMs: time.Since(start).Milliseconds()}
		s.audit(r.Context(), auditEntry{
			endpoint:   "/api/notify-owner",
			params:     map[string]any{"urgency": req.Urgency, "queued": true},
			result:     "queued-quiet-hours",
			durationMs: resp.LatencyMs,
		})
		writeJSON(w, http.StatusOK, resp)
		return
	}

	budget := &waPingBudget{db: s.presetDB, limitPH: s.cfg.WhatsAppPingsPerHour}
	ok, err := budget.CheckAndRecordPing("/api/notify-owner", req.Urgency)
	if err != nil {
		log.Printf("notify-owner: budget check: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "budget check failed: "+err.Error(), true)
		return
	}
	if !ok {
		deliverAt := now.Add(time.Hour).Truncate(time.Hour)
		_ = EnqueuePing(s.presetDB, req.Message, req.Urgency, req.DeeplinkToInboxFile, deliverAt)
		resp := NotifyResponse{Delivered: false, LatencyMs: time.Since(start).Milliseconds()}
		s.audit(r.Context(), auditEntry{
			endpoint:   "/api/notify-owner",
			params:     map[string]any{"urgency": req.Urgency, "queued": true},
			result:     "queued-budget-exceeded",
			durationMs: resp.LatencyMs,
		})
		writeJSON(w, http.StatusOK, resp)
		return
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

	draftPayload, _ := json.Marshal(map[string]string{
		"jid":     selfJID,
		"message": text,
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
func (s *Server) resolveSelfJID() (string, error) {
	if jid := os.Getenv("PROSPECT_SELF_JID"); jid != "" {
		return jid, nil
	}
	statusBody, err := s.bridgeRequest("GET", s.cfg.BridgeBaseURL+"/api/status", nil)
	if err != nil {
		return "", fmt.Errorf("bridge status: %w", err)
	}
	var status struct {
		JID string `json:"jid"`
	}
	if err := json.Unmarshal(statusBody, &status); err != nil || status.JID == "" {
		return "", fmt.Errorf("bridge status jid missing (body: %s)", string(statusBody))
	}
	return status.JID, nil
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
