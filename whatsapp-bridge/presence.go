package main

import (
	"encoding/json"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Presence endpoints: low-consequence state changes that do not need a
// draft+confirm pattern.
//
//  POST /api/presence/mark_read      body: {"chat_jid": "...", "message_ids": ["id1","id2"]}
//  POST /api/presence/typing         body: {"chat_jid": "...", "state": "composing"|"paused"}
//  POST /api/presence/online         body: {"online": true|false}
//
// These fire a single whatsmeow call each. Still gated by IsConnected() so
// we do not attempt to operate against a dead socket.

type markReadRequest struct {
	ChatJID    string   `json:"chat_jid"`
	MessageIDs []string `json:"message_ids"`
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	var req markReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON", Details: err.Error()})
		return
	}
	if req.ChatJID == "" || len(req.MessageIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "chat_jid and non-empty message_ids required"})
		return
	}
	if !s.bridge.IsConnected() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "bridge not connected"})
		return
	}
	chatJID, err := types.ParseJID(req.ChatJID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid chat_jid", Details: err.Error()})
		return
	}

	ids := make([]types.MessageID, 0, len(req.MessageIDs))
	for _, id := range req.MessageIDs {
		ids = append(ids, types.MessageID(id))
	}
	if err := s.bridge.client.MarkRead(r.Context(), ids, time.Now(), chatJID, types.EmptyJID); err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "mark read failed", Details: err.Error()})
		return
	}

	_, _ = s.db.ExecContext(r.Context(), `UPDATE chats SET unread_count = 0 WHERE jid = ?`, req.ChatJID)

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "chat_jid": req.ChatJID, "marked": len(req.MessageIDs)})
}

type typingRequest struct {
	ChatJID string `json:"chat_jid"`
	State   string `json:"state"` // "composing" | "paused"
}

func (s *Server) handleTyping(w http.ResponseWriter, r *http.Request) {
	var req typingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON", Details: err.Error()})
		return
	}
	if req.ChatJID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "chat_jid required"})
		return
	}
	if req.State == "" {
		req.State = "composing"
	}
	if req.State != "composing" && req.State != "paused" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "state must be 'composing' or 'paused'"})
		return
	}
	if !s.bridge.IsConnected() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "bridge not connected"})
		return
	}

	chatJID, err := types.ParseJID(req.ChatJID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid chat_jid", Details: err.Error()})
		return
	}

	presence := types.ChatPresenceComposing
	if req.State == "paused" {
		presence = types.ChatPresencePaused
	}
	if err := s.bridge.client.SendChatPresence(r.Context(), chatJID, presence, types.ChatPresenceMediaText); err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "typing signal failed", Details: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "chat_jid": req.ChatJID, "state": req.State})
}

type onlineRequest struct {
	Online bool `json:"online"`
}

func (s *Server) handleOnline(w http.ResponseWriter, r *http.Request) {
	var req onlineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON", Details: err.Error()})
		return
	}
	if !s.bridge.IsConnected() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "bridge not connected"})
		return
	}

	presence := types.PresenceUnavailable
	if req.Online {
		presence = types.PresenceAvailable
	}
	if err := s.bridge.client.SendPresence(r.Context(), presence); err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "presence failed", Details: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "online": req.Online})
}
