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
	cfg        *Config
	db         *sql.DB
	bridge     *Bridge
	backfiller *TranscriptBackfiller
	mux        *http.ServeMux
	server     *http.Server
}

func NewServer(cfg *Config, db *sql.DB, bridge *Bridge, backfiller *TranscriptBackfiller) *Server {
	s := &Server{cfg: cfg, db: db, bridge: bridge, backfiller: backfiller, mux: http.NewServeMux()}
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

	s.mux.HandleFunc("POST /api/presence/mark_read", s.handleMarkRead)
	s.mux.HandleFunc("POST /api/presence/typing", s.handleTyping)
	s.mux.HandleFunc("POST /api/presence/online", s.handleOnline)

	s.mux.HandleFunc("POST /api/admin/backfill-transcripts", s.handleBackfillTranscripts)
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
	Status         string         `json:"status"`
	Version        string         `json:"version"`
	DBEncrypted    bool           `json:"db_encrypted"`
	SchemaVer      int            `json:"schema_version"`
	Timestamp      int64          `json:"timestamp"`
	Transcription  map[string]any `json:"transcription,omitempty"`
	AliasCoverage  AliasCoverage  `json:"alias_coverage"`
}

func (s *Server) handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	var schemaVer int
	_ = s.db.QueryRowContext(r.Context(), "SELECT MAX(version) FROM schema_version").Scan(&schemaVer)

	// Transcription health: surface the actual data points an operator
	// needs to know whether voice notes are flowing through. Hides nothing.
	tx := map[string]any{
		"backend":     s.cfg.WhisperBackend,
		"language":    s.cfg.WhisperLanguage,
		"model_path":  s.cfg.WhisperModelPath,
		"bin_path":    s.cfg.WhisperBinPath,
	}
	var pending int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM messages WHERE type IN ('voice','audio') AND voice_note_transcript IS NULL AND media_key IS NOT NULL`,
	).Scan(&pending)
	tx["pending_with_keys"] = pending

	var orphans int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM messages WHERE type IN ('voice','audio') AND voice_note_transcript IS NULL AND media_key IS NULL`,
	).Scan(&orphans)
	tx["orphan_no_keys"] = orphans

	var lastTranscript int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(MAX(voice_note_transcript_at), 0) FROM messages WHERE voice_note_transcript IS NOT NULL`,
	).Scan(&lastTranscript)
	tx["last_transcript_at"] = lastTranscript

	if s.backfiller != nil {
		for k, v := range s.backfiller.Stats() {
			tx["sweeper_"+k] = v
		}
	}

	// Alias coverage. SuspiciousLIDPhones > 0 means the recurring-class bug
	// is present in the data: at least one @lid contact has its `phone`
	// column populated with the LID number itself (the legacy ingestion
	// pattern). Operators can spot regressions immediately from healthcheck.
	cov, covErr := computeAliasCoverage(r.Context(), s.db)
	if covErr != nil {
		log.Printf("healthcheck: alias coverage query failed: %v", covErr)
	}

	writeJSON(w, http.StatusOK, healthcheckResponse{
		Status:        "ok",
		Version:       "0.3.0",
		DBEncrypted:   s.cfg.EncryptDB,
		SchemaVer:     schemaVer,
		Timestamp:     time.Now().Unix(),
		Transcription: tx,
		AliasCoverage: cov,
	})
}

// handleBackfillTranscripts fires one immediate sweep. Returns the count
// enqueued. The periodic sweeper continues to run on its own cadence;
// this is purely an on-demand trigger (e.g. operator just received voice
// notes during an outage and wants them transcribed now).
func (s *Server) handleBackfillTranscripts(w http.ResponseWriter, r *http.Request) {
	if s.backfiller == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "backfiller not configured"})
		return
	}
	n, err := s.backfiller.SweepOnce(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "sweep failed", Details: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enqueued":  n,
		"timestamp": time.Now().Unix(),
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
	Messages    []messageRow `json:"messages"`
	Count       int          `json:"count"`
	Chat        *chatRow     `json:"chat,omitempty"`
	MergedJIDs  []string     `json:"merged_jids,omitempty"`
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

	// Expand the requested JID through jid_aliases so a query for either the
	// LID form or the @s.whatsapp.net form returns the merged history.
	// Without this, a contact whose recent traffic moved to LID looks silent
	// when queried by their legacy phone JID.
	allJIDs, err := resolveAliases(r.Context(), s.db, chatJID)
	if err != nil {
		log.Printf("handleListMessages: alias resolve failed (continuing with single jid): %v", err)
		allJIDs = []string{chatJID}
	}

	// Pick the chat row to return: prefer the alias whose chats row has the
	// most recent activity, so callers get a sensible "current" name + preview.
	var chat chatRow
	{
		args := jidsToArgs(allJIDs)
		query := fmt.Sprintf(`
			SELECT jid, chat_type, COALESCE(name, ''), COALESCE(last_message_time, 0),
			       COALESCE(last_message_preview, ''), unread_count
			FROM chats WHERE jid IN (%s)
			ORDER BY last_message_time DESC
			LIMIT 1
		`, inClausePlaceholders(len(allJIDs)))
		_ = s.db.QueryRowContext(r.Context(), query, args...).Scan(
			&chat.JID, &chat.ChatType, &chat.Name, &chat.LastMessageTime, &chat.LastMessagePreview, &chat.UnreadCount,
		)
	}

	args := append(jidsToArgs(allJIDs), limit)
	query := fmt.Sprintf(`
		SELECT id, chat_jid, COALESCE(sender_jid, ''), COALESCE(sender_display, ''), timestamp, type,
		       COALESCE(scrubbed_text, COALESCE(content_text, '')),
		       is_from_me, voice_note_transcript, COALESCE(quoted_message_id, '')
		FROM messages
		WHERE chat_jid IN (%s)
		ORDER BY timestamp DESC
		LIMIT ?
	`, inClausePlaceholders(len(allJIDs)))

	rows, err := s.db.QueryContext(r.Context(), query, args...)
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
	if len(allJIDs) > 1 {
		out.MergedJIDs = allJIDs
	}
	writeJSON(w, http.StatusOK, out)
}

type contactRow struct {
	JID            string   `json:"jid"`
	LID            string   `json:"lid,omitempty"`
	Phone          string   `json:"phone,omitempty"`
	PushName       string   `json:"push_name"`
	VerifiedName   string   `json:"verified_name,omitempty"`
	IsBusiness     bool     `json:"is_business"`
	Aliases        []string `json:"aliases,omitempty"` // other JIDs known to refer to the same human
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

	// Initial match by name/phone/lid. We then expand each match through
	// jid_aliases so callers see every JID known to refer to the same human
	// (LID + phone-JID forms). Without this, a name search returns only the
	// row whose stored push_name happens to match exactly, hiding the alias.
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

	// Buffer initial matches; we'll expand and dedupe before responding.
	type match struct {
		c    contactRow
		isBiz int
	}
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.c.JID, &m.c.LID, &m.c.Phone, &m.c.PushName, &m.c.VerifiedName, &m.isBiz); err != nil {
			continue
		}
		m.c.IsBusiness = m.isBiz == 1
		matches = append(matches, m)
	}
	rows.Close()

	// Expand: for each match, gather aliases and pull any contact rows we
	// don't already have. Dedupe by JID.
	seen := map[string]bool{}
	out := contactListResponse{Contacts: []contactRow{}}

	for _, m := range matches {
		if seen[m.c.JID] {
			continue
		}
		aliases, _ := resolveAliases(r.Context(), s.db, m.c.JID)
		// First slot in resolveAliases is the JID itself; the rest are aliases.
		if len(aliases) > 1 {
			m.c.Aliases = aliases[1:]
		}
		seen[m.c.JID] = true
		out.Contacts = append(out.Contacts, m.c)

		// Also surface alias rows as separate contacts so callers can list_messages
		// against either form. They carry the original-row's aliases inverted.
		for _, alt := range aliases[1:] {
			if seen[alt] {
				continue
			}
			altRow, ok := loadContactRow(r.Context(), s.db, alt)
			if !ok {
				continue
			}
			altAliases, _ := resolveAliases(r.Context(), s.db, alt)
			if len(altAliases) > 1 {
				altRow.Aliases = altAliases[1:]
			}
			seen[alt] = true
			out.Contacts = append(out.Contacts, altRow)
		}
	}

	out.Count = len(out.Contacts)
	writeJSON(w, http.StatusOK, out)
}

// loadContactRow fetches a single contacts row by JID. Returns (row, true)
// when present. Used by handleSearchContacts to surface alias rows that did
// not match the name query directly.
func loadContactRow(ctx context.Context, db *sql.DB, jid string) (contactRow, bool) {
	var c contactRow
	var isBiz int
	err := db.QueryRowContext(ctx, `
		SELECT jid, COALESCE(lid, ''), COALESCE(phone, ''), COALESCE(push_name, ''), COALESCE(verified_name, ''), is_business
		FROM contacts WHERE jid = ?
	`, jid).Scan(&c.JID, &c.LID, &c.Phone, &c.PushName, &c.VerifiedName, &isBiz)
	if err != nil {
		return c, false
	}
	c.IsBusiness = isBiz == 1
	return c, true
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
