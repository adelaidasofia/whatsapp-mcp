package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Server is the HTTP REST API the Python MCP server consumes.
// It runs on the loopback interface only (enforced in config.Validate()).
type Server struct {
	cfg    *Config
	db     *sql.DB
	mux    *http.ServeMux
	server *http.Server
}

func NewServer(cfg *Config, db *sql.DB) *Server {
	s := &Server{cfg: cfg, db: db, mux: http.NewServeMux()}
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

	// Core read endpoints. Full implementations land in handlers.go in the next commit.
	s.mux.HandleFunc("GET /api/chats", s.handleListChats)
	s.mux.HandleFunc("GET /api/messages", s.handleListMessages)
	s.mux.HandleFunc("GET /api/contacts/search", s.handleSearchContacts)

	// Send flow (two-step draft then confirm).
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
		Version:     "0.1.0",
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
	// Bridge connection / authentication state will be populated once the whatsmeow client
	// is wired up in bridge.go. For v0.1.0 the response reflects config only.
	writeJSON(w, http.StatusOK, statusResponse{
		Connected:         false, // pending bridge.go
		Authenticated:     false, // pending bridge.go
		WhisperBackend:    s.cfg.WhisperBackend,
		VaultCRMEnabled:   s.cfg.VaultCRMPath != "",
		CaptureCalls:      s.cfg.CaptureCalls,
		AutoDownloadMedia: s.cfg.AutoDownloadMedia,
	})
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "list_chats", "wiring to SQLite lands in next commit")
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "list_messages", "wiring to SQLite lands in next commit")
}

func (s *Server) handleSearchContacts(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "search_contacts", "wiring to SQLite lands in next commit")
}

func (s *Server) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "create_draft", "send flow lands in commit 3 (after whatsmeow client integration)")
}

func (s *Server) handleConfirmSend(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "confirm_send", "send flow lands in commit 3 (after whatsmeow client integration)")
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
		Error:   fmt.Sprintf("%s: not implemented in v0.1.0", tool),
		Details: details,
	})
}
