package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Server is the HTTP REST API the Python MCP server consumes.
// Runs on the loopback interface only (enforced in config.Validate()).
type Server struct {
	cfg    *Config
	db     *sql.DB
	bridge *Bridge
	mux    *http.ServeMux
	server *http.Server
}

func NewServer(cfg *Config, db *sql.DB, bridge *Bridge) *Server {
	s := &Server{cfg: cfg, db: db, bridge: bridge, mux: http.NewServeMux()}
	s.registerRoutes()
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.BridgeHost, cfg.BridgePort),
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthcheck", s.handleHealthcheck)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)

	s.mux.HandleFunc("GET /api/chats", s.handleListChats)
	s.mux.HandleFunc("GET /api/messages", s.handleListMessages)
	s.mux.HandleFunc("GET /api/contacts/search", s.handleSearchContacts)

	s.mux.HandleFunc("POST /api/sends", s.handleCreateDraft)
	s.mux.HandleFunc("POST /api/sends/{draft_id}/confirm", s.handleConfirmSend)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// --- handlers --------------------------------------------------------------

type healthcheckResponse struct {
	Status      string `json:"status"`
	Version     string `json:"version"`
	DBEncrypted bool   `json:"db_encrypted"`
	SchemaVer   int    `json:"schema_version"`
	Timestamp   int64  `json:"timestamp"`
}

func (s *Server) handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	var schemaVer int
	_ = s.db.QueryRowContext(r.Context(), "SELECT MAX(version) FROM schema_version").Scan(&schemaVer)

	writeJSON(w, http.StatusOK, healthcheckResponse{
		Status:      "ok",
		Version:     "0.2.0",
		DBEncrypted: s.cfg.EncryptDB,
		SchemaVer:   schemaVer,
		Timestamp:   time.Now().Unix(),
	})
}

type statusResponse struct {
	Connected         bool   `json:"connected"`
	Authenticated     bool   `json:"authenticated"`
	DeviceJID         string `json:"device_jid,omitempty"`
	LastSyncTime      int64  `json:"last_sync_time,omitempty"`
	WhisperBackend    string `json:"whisper_backend"`
	VaultCRMEnabled   bool   `json:"vault_crm_enabled"`
	CaptureCalls      bool   `json:"capture_calls"`
	AutoDownloadMedia bool   `json:"auto_download_media"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	connected, authed, deviceJID, lastSync := s.bridge.Status()
	writeJSON(w, http.StatusOK, statusResponse{
		Connected:         connected,
		Authenticated:     authed,
		DeviceJID:         deviceJID,
		LastSyncTime:      lastSync,
		WhisperBackend:    s.cfg.WhisperBackend,
		VaultCRMEnabled:   s.cfg.VaultCRMPath != "",
		CaptureCalls:      s.cfg.CaptureCalls,
		AutoDownloadMedia: s.cfg.AutoDownloadMedia,
	})
}

type chatRow struct {
	JID              string `json:"jid"`
	ChatType         string `json:"chat_type"`
	Name             string `json:"name"`
	LastMessageTime  int64  `json:"last_message_time"`
	LastMessagePreview string `json:"last_message_preview"`
	UnreadCount      int    `json:"unread_count"`
}

type chatListResponse struct {
	Chats []chatRow `json:"chats"`
	Count int       `json:"count"`
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 20)
	if limit > 200 {
		limit = 200
	}
	offset := parseIntDefault(q.Get("offset"), 0)
	unreadOnly := q.Get("unread_only") == "true"

	sqlStr := `SELECT jid, chat_type, COALESCE(name, ''), COALESCE(last_message_time, 0), COALESCE(last_message_preview, ''), unread_count
	           FROM chats`
	args := []any{}
	if unreadOnly {
		sqlStr += ` WHERE unread_count > 0`
	}
	sqlStr += ` ORDER BY last_message_time DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(r.Context(), sqlStr, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "query failed", Details: err.Error()})
		return
	}
	defer rows.Close()

	out := chatListResponse{Chats: []chatRow{}}
	for rows.Next() {
		var c chatRow
		if err := rows.Scan(&c.JID, &c.ChatType, &c.Name, &c.LastMessageTime, &c.LastMessagePreview, &c.UnreadCount); err != nil {
			continue
		}
		out.Chats = append(out.Chats, c)
	}
	out.Count = len(out.Chats)
	writeJSON(w, http.StatusOK, out)
}

type messageRow struct {
	ID             string  `json:"id"`
	ChatJID        string  `json:"chat_jid"`
	SenderJID      string  `json:"sender_jid"`
	SenderDisplay  string  `json:"sender_display"`
	Timestamp      int64   `json:"timestamp"`
	Type           string  `json:"type"`
	ContentText    string  `json:"content_text"`
	IsFromMe       bool    `json:"is_from_me"`
	Transcript     *string `json:"voice_note_transcript,omitempty"`
	QuotedID       string  `json:"quoted_message_id,omitempty"`
}

type messageListResponse struct {
	Messages []messageRow `json:"messages"`
	Count    int          `json:"count"`
	Chat     *chatRow     `json:"chat,omitempty"`
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	chatJID := q.Get("chat_jid")
	if chatJID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "chat_jid required"})
		return
	}
	limit := parseIntDefault(q.Get("limit"), 20)
	if limit > 500 {
		limit = 500
	}

	var chat chatRow
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT jid, chat_type, COALESCE(name, ''), COALESCE(last_message_time, 0), COALESCE(last_message_preview, ''), unread_count
		 FROM chats WHERE jid = ?`, chatJID,
	).Scan(&chat.JID, &chat.ChatType, &chat.Name, &chat.LastMessageTime, &chat.LastMessagePreview, &chat.UnreadCount)

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, chat_jid, COALESCE(sender_jid, ''), COALESCE(sender_display, ''), timestamp, type,
		       COALESCE(scrubbed_text, COALESCE(content_text, '')),
		       is_from_me, voice_note_transcript, COALESCE(quoted_message_id, '')
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, chatJID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "query failed", Details: err.Error()})
		return
	}
	defer rows.Close()

	out := messageListResponse{Messages: []messageRow{}}
	for rows.Next() {
		var m messageRow
		var fromMe int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderDisplay, &m.Timestamp, &m.Type,
			&m.ContentText, &fromMe, &m.Transcript, &m.QuotedID); err != nil {
			continue
		}
		m.IsFromMe = fromMe == 1
		out.Messages = append(out.Messages, m)
	}
	out.Count = len(out.Messages)
	if chat.JID != "" {
		out.Chat = &chat
	}
	writeJSON(w, http.StatusOK, out)
}

type contactRow struct {
	JID            string `json:"jid"`
	LID            string `json:"lid,omitempty"`
	Phone          string `json:"phone,omitempty"`
	PushName       string `json:"push_name"`
	VerifiedName   string `json:"verified_name,omitempty"`
	IsBusiness     bool   `json:"is_business"`
}

type contactListResponse struct {
	Contacts []contactRow `json:"contacts"`
	Count    int          `json:"count"`
}

func (s *Server) handleSearchContacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("q")
	limit := parseIntDefault(q.Get("limit"), 10)
	if limit > 100 {
		limit = 100
	}

	if query == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "q parameter required"})
		return
	}
	norm := Normalize(query)

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT jid, COALESCE(lid, ''), COALESCE(phone, ''), COALESCE(push_name, ''), COALESCE(verified_name, ''), is_business
		FROM contacts
		WHERE normalized_name LIKE ? OR phone LIKE ? OR lid LIKE ?
		ORDER BY updated_at DESC
		LIMIT ?
	`, "%"+norm+"%", "%"+query+"%", "%"+query+"%", limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "query failed", Details: err.Error()})
		return
	}
	defer rows.Close()

	out := contactListResponse{Contacts: []contactRow{}}
	for rows.Next() {
		var c contactRow
		var isBiz int
		if err := rows.Scan(&c.JID, &c.LID, &c.Phone, &c.PushName, &c.VerifiedName, &isBiz); err != nil {
			continue
		}
		c.IsBusiness = isBiz == 1
		out.Contacts = append(out.Contacts, c)
	}
	out.Count = len(out.Contacts)
	writeJSON(w, http.StatusOK, out)
}

// Create-draft and confirm-send land in commit 3 once the whatsmeow send flow is wired.
func (s *Server) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "create_draft", "send flow lands in commit 3")
}

func (s *Server) handleConfirmSend(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "confirm_send", "send flow lands in commit 3")
}

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("encode response: %v", err)
	}
}

type errorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

func notImplemented(w http.ResponseWriter, tool, details string) {
	writeJSON(w, http.StatusNotImplemented, errorResponse{
		Error:   fmt.Sprintf("%s: not implemented in current version", tool),
		Details: details,
	})
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
