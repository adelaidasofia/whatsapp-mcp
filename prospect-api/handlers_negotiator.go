package main

import (
	"log"
	"net/http"
	"time"
)

// NegotiatorRequest is the public surface for /api/get-negotiator-terms.
type NegotiatorRequest struct {
	Category    string `json:"category"`
	GuestContext string `json:"guestContext,omitempty"`
}

// NegotiatorResponse is the response for /api/get-negotiator-terms.
type NegotiatorResponse struct {
	CounterConditions []string `json:"counterConditions"`
	FloorLanguage     string   `json:"floorLanguage,omitempty"`
	GracefulExit      string   `json:"gracefulExit"`
	PlaybookVersion   string   `json:"playbookVersion"`
	LatencyMs         int64    `json:"latencyMs"`
}

// validNegotiatorCategories is the exhaustive set of categories the bridge supports.
var validNegotiatorCategories = map[string]bool{
	"speaking_ask":         true,
	"press_media":          true,
	"partnership":          true,
	"introduction_request": true,
	"consulting_misfit":    true,
}

// handleGetNegotiatorTerms serves POST /api/get-negotiator-terms.
//
// Reads the Negotiator Playbook markdown from disk on every call (no cache beyond
// filesystem — updates are immediately live). Returns the section for the requested
// category or 404 with code "playbook-section-missing" if absent.
func (s *Server) handleGetNegotiatorTerms(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req NegotiatorRequest
	if err := readJSONBody(r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.Category == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "category is required", false)
		return
	}
	if !validNegotiatorCategories[req.Category] {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest,
			"category must be one of: speaking_ask, press_media, partnership, introduction_request, consulting_misfit", false)
		return
	}

	pbPath := s.cfg.NegotiatorPlaybookPath
	if pbPath == "" {
		writeError(w, http.StatusServiceUnavailable, ErrServiceUnavailab,
			"PROSPECT_NEGOTIATOR_PLAYBOOK_PATH not configured", false)
		return
	}

	sec, err := ParseNegotiatorPlaybook(pbPath, req.Category)
	if err != nil {
		log.Printf("get-negotiator-terms: parse playbook: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "playbook parse error: "+err.Error(), true)
		return
	}
	if sec == nil {
		writeError(w, http.StatusNotFound, "playbook-section-missing",
			"playbook has no section for category: "+req.Category, false)
		return
	}

	latencyMs := time.Since(start).Milliseconds()
	resp := NegotiatorResponse{
		CounterConditions: sec.CounterConds,
		FloorLanguage:     sec.FloorLanguage,
		GracefulExit:      sec.GracefulExit,
		PlaybookVersion:   sec.PlaybookVersion,
		LatencyMs:         latencyMs,
	}
	if resp.CounterConditions == nil {
		resp.CounterConditions = []string{}
	}

	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/get-negotiator-terms",
		params:     map[string]any{"category": req.Category},
		result:     "ok",
		durationMs: latencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}
