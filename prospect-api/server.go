package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Server is the prospect-api HTTP service.
//
// Binds to 127.0.0.1 only. Public exposure is via Cloudflare Tunnel.
// All endpoints require bearer auth (with /api/preset upgraded to admin auth).
type Server struct {
	cfg       *Config
	messageDB *sql.DB // read-only access to whatsapp-bridge's SQLCipher DB
	presetDB  *sql.DB // local preset + audit + ping budget DB
	crm       *CRMStore
	mux       *http.ServeMux
	server    *http.Server

	// Per-token sliding-window rate limiter (60 req/min default).
	tokenLimiter *rateLimiter
	// Per-phone limiter for lookup endpoint (anti-enumeration).
	phoneLimiter *rateLimiter
}

func NewServer(cfg *Config, messageDB, presetDB *sql.DB, crm *CRMStore) *Server {
	s := &Server{
		cfg:          cfg,
		messageDB:    messageDB,
		presetDB:     presetDB,
		crm:          crm,
		mux:          http.NewServeMux(),
		tokenLimiter: newRateLimiter(cfg.GlobalRateLimitPerMin, time.Minute),
		// Per-phone: 5/min strictly, prevents an attacker probing 60 different phones.
		phoneLimiter: newRateLimiter(5, time.Minute),
	}
	s.registerRoutes()
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.BindHost, cfg.BindPort),
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

func (s *Server) registerRoutes() {
	// Health: no auth, basic up-check for Cloudflare healthcheck.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Phase A — bearer-auth required.
	s.mux.HandleFunc("POST /api/lookup-prospect",
		withClientIP(s.authBearer(s.rateLimit(s.handleLookupProspect))))
	s.mux.HandleFunc("POST /api/pull-context",
		withClientIP(s.authBearer(s.rateLimit(s.handlePullContext))))
	s.mux.HandleFunc("POST /api/update-crm",
		withClientIP(s.authBearer(s.rateLimit(s.handleUpdateCRM))))
	s.mux.HandleFunc("POST /api/check-preset",
		withClientIP(s.authBearer(s.rateLimit(s.handleCheckPreset))))

	// Admin-auth only (separate token). Higher trust.
	s.mux.HandleFunc("POST /api/preset",
		withClientIP(s.authAdmin(s.rateLimit(s.handleCreatePreset))))

	// Phase B endpoints (Sprints 7-10) land below in subsequent commits.
}

func (s *Server) ListenAndServe(_ context.Context) error {
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

// --- Healthz ---------------------------------------------------------------

type healthzResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Timestamp int64  `json:"timestamp"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// Confirm DBs are reachable.
	if err := s.messageDB.PingContext(r.Context()); err != nil {
		log.Printf("healthz: messageDB ping failed: %v", err)
		writeError(w, http.StatusServiceUnavailable, ErrServiceUnavailab, "message db unreachable", true)
		return
	}
	if err := s.presetDB.PingContext(r.Context()); err != nil {
		log.Printf("healthz: presetDB ping failed: %v", err)
		writeError(w, http.StatusServiceUnavailable, ErrServiceUnavailab, "preset db unreachable", true)
		return
	}
	writeJSON(w, http.StatusOK, healthzResponse{
		Status:    "ok",
		Version:   "0.1.0",
		Timestamp: time.Now().Unix(),
	})
}
