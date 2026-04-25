package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenKind tags an authorized request as either bearer (concierge) or admin (preset writer).
type tokenKind string

const (
	tokenKindBearer tokenKind = "bearer"
	tokenKindAdmin  tokenKind = "admin"
)

type ctxKey string

const (
	ctxKeyToken    ctxKey = "tokenKind"
	ctxKeyClientIP ctxKey = "clientIP"
)

// authBearer requires the BridgeAuthToken in the Authorization header.
func (s *Server) authBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuthHeader(w, r, s.cfg.BridgeAuthToken) {
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyToken, tokenKindBearer)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// authAdmin requires the AdminToken (used only by /api/preset).
func (s *Server) authAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuthHeader(w, r, s.cfg.AdminToken) {
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyToken, tokenKindAdmin)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

func (s *Server) checkAuthHeader(w http.ResponseWriter, r *http.Request, expected string) bool {
	got := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(got, prefix) {
		writeError(w, http.StatusUnauthorized, ErrUnauthorized, "missing or malformed Authorization header", false)
		return false
	}
	provided := got[len(prefix):]
	if !constantTimeEq(provided, expected) {
		writeError(w, http.StatusUnauthorized, ErrUnauthorized, "invalid token", false)
		return false
	}
	if s.cfg.RequireCloudflareAccess {
		if r.Header.Get("Cf-Access-Authenticated-User-Email") == "" && r.Header.Get("Cf-Access-Jwt-Assertion") == "" {
			writeError(w, http.StatusForbidden, ErrForbidden, "cloudflare access trust required", false)
			return false
		}
	}
	return true
}

// constantTimeEq compares two strings in constant time relative to length.
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// --- Rate limiting ---------------------------------------------------------

// rateLimiter is a small per-key sliding-window limiter.
// Two scopes:
//   - per token (60 req/min default, applies to all endpoints)
//   - per phone (lower; lookup-prospect only, prevents enumeration)
type rateLimiter struct {
	mu       sync.Mutex
	hits     map[string][]time.Time
	limit    int
	window   time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
	}
}

func (r *rateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-r.window)
	old := r.hits[key]
	kept := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= r.limit {
		r.hits[key] = kept
		return false
	}
	kept = append(kept, now)
	r.hits[key] = kept
	return true
}

func (s *Server) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if !s.tokenLimiter.Allow(token) {
			writeError(w, http.StatusTooManyRequests, ErrRateLimited,
				"per-token rate limit exceeded (60/min default)", true)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// --- Audit -----------------------------------------------------------------

type auditEntry struct {
	endpoint   string
	tokenKind  tokenKind
	clientIP   string
	params     any
	result     string
	durationMs int64
	err        string
}

func (s *Server) audit(ctx context.Context, e auditEntry) {
	tk := e.tokenKind
	if tk == "" {
		if v, ok := ctx.Value(ctxKeyToken).(tokenKind); ok {
			tk = v
		}
	}
	cip := e.clientIP
	if cip == "" {
		if v, ok := ctx.Value(ctxKeyClientIP).(string); ok {
			cip = v
		}
	}
	pj, _ := json.Marshal(e.params)
	_, err := s.presetDB.ExecContext(ctx, `
		INSERT INTO audit_log (ts, endpoint, token_kind, client_ip, params_json, result_summary, duration_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, time.Now().Unix(), e.endpoint, string(tk), cip, string(pj), e.result, e.durationMs, e.err)
	if err != nil {
		log.Printf("audit insert failed: %v", err)
	}
}

// --- Helpers ---------------------------------------------------------------

func clientIP(r *http.Request) string {
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
		return cf
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

// withClientIP injects the client IP into the request context for downstream audit.
func withClientIP(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyClientIP, clientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// readJSONBody decodes the request body into dst, capping at maxBytes for sanity.
func readJSONBody(r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// presetDBHandle is here just to make the audit() context-receiver cleaner.
// The Server struct holds the actual handle.
type presetDBHandle struct {
	*sql.DB
}
