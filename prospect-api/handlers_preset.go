package main

import (
	"net/http"
	"time"
)

// CreatePresetRequest pre-warms a guest. Admin-token only.
type CreatePresetRequest struct {
	GuestEmail    string `json:"guestEmail,omitempty"`
	GuestPhone    string `json:"guestPhone,omitempty"`
	GuestName     string `json:"guestName"`
	Context       string `json:"context"`
	Relationship  string `json:"relationship"`
	ExpiresInDays int    `json:"expiresInDays"`
}

type CreatePresetResponse struct {
	PresetID   string `json:"presetId"`
	ExpiresAt  string `json:"expiresAt"`
	PreWarmUrl string `json:"preWarmUrl"`
	LatencyMs  int64  `json:"latencyMs"`
}

func (s *Server) handleCreatePreset(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req CreatePresetRequest
	if err := readJSONBody(r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.ExpiresInDays < 1 || req.ExpiresInDays > 30 {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "expiresInDays must be between 1 and 30", false)
		return
	}
	expiresAt := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).Unix()

	id, err := CreatePreset(r.Context(), s.presetDB, Preset{
		GuestEmail:   req.GuestEmail,
		GuestPhone:   req.GuestPhone,
		GuestName:    req.GuestName,
		Context:      req.Context,
		Relationship: req.Relationship,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, err.Error(), false)
		return
	}

	resp := CreatePresetResponse{
		PresetID:   id,
		ExpiresAt:  time.Unix(expiresAt, 0).UTC().Format(time.RFC3339),
		PreWarmUrl: "https://concierge.diazroa.com/?invite=" + id,
		LatencyMs:  time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/preset",
		params:     map[string]any{"guestName": req.GuestName, "rel": req.Relationship, "ttlDays": req.ExpiresInDays},
		result:     "preset_created:" + id,
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

// CheckPresetRequest is called by the concierge on guest arrival.
type CheckPresetRequest struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	Name  string `json:"name,omitempty"`
}

type CheckPresetResponse struct {
	Matched   bool         `json:"matched"`
	Preset    *PresetView  `json:"preset,omitempty"`
	LatencyMs int64        `json:"latencyMs"`
}

type PresetView struct {
	GuestName    string `json:"guestName"`
	Context      string `json:"context"`
	Relationship string `json:"relationship"`
	ExpiresAt    string `json:"expiresAt"`
}

func (s *Server) handleCheckPreset(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req CheckPresetRequest
	if err := readJSONBody(r, &req, 2048); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.Email == "" && req.Phone == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "email or phone required", false)
		return
	}

	consumer := clientIP(r)
	preset, err := CheckPreset(r.Context(), s.presetDB, req.Email, req.Phone, req.Name, consumer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "preset lookup failed: "+err.Error(), true)
		return
	}
	resp := CheckPresetResponse{
		Matched:   preset != nil,
		LatencyMs: time.Since(start).Milliseconds(),
	}
	if preset != nil {
		resp.Preset = &PresetView{
			GuestName:    preset.GuestName,
			Context:      preset.Context,
			Relationship: preset.Relationship,
			ExpiresAt:    time.Unix(preset.ExpiresAt, 0).UTC().Format(time.RFC3339),
		}
	}

	result := "no-match"
	if resp.Matched {
		result = "match:" + preset.PresetID
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/check-preset",
		params:     map[string]any{"hasEmail": req.Email != "", "hasPhone": req.Phone != ""},
		result:     result,
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

// GetPresetRequest is called by the concierge when the guest arrives via ?invite=TOKEN.
// Bearer-auth (not admin) since it's read-only and the token itself is the credential.
type GetPresetRequest struct {
	PresetID string `json:"presetId"`
}

type GetPresetResponse struct {
	Found     bool        `json:"found"`
	Preset    *PresetView `json:"preset,omitempty"`
	LatencyMs int64       `json:"latencyMs"`
}

func (s *Server) handleGetPreset(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req GetPresetRequest
	if err := readJSONBody(r, &req, 1024); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.PresetID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "presetId required", false)
		return
	}

	preset, err := GetPresetByID(r.Context(), s.presetDB, req.PresetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "preset lookup failed: "+err.Error(), true)
		return
	}

	resp := GetPresetResponse{
		Found:     preset != nil,
		LatencyMs: time.Since(start).Milliseconds(),
	}
	if preset != nil {
		resp.Preset = &PresetView{
			GuestName:    preset.GuestName,
			Context:      preset.Context,
			Relationship: preset.Relationship,
			ExpiresAt:    time.Unix(preset.ExpiresAt, 0).UTC().Format(time.RFC3339),
		}
	}

	result := "not-found"
	if resp.Found {
		result = "found:" + preset.PresetID
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/get-preset",
		params:     map[string]any{"presetId": req.PresetID},
		result:     result,
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

// ConsumePresetRequest marks a preset consumed (single-use). Bearer-auth.
// Concierge calls this after a successful create_booking that originated from
// a ?invite= URL.
type ConsumePresetRequest struct {
	PresetID       string `json:"presetId"`
	ConsumedReason string `json:"consumedReason,omitempty"`
}

type ConsumePresetResponse struct {
	Consumed   bool   `json:"consumed"`
	ConsumedAt string `json:"consumedAt,omitempty"`
	LatencyMs  int64  `json:"latencyMs"`
}

func (s *Server) handleConsumePreset(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req ConsumePresetRequest
	if err := readJSONBody(r, &req, 1024); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.PresetID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "presetId required", false)
		return
	}

	consumer := req.ConsumedReason
	if consumer == "" {
		consumer = clientIP(r)
	}
	consumedAt, err := ConsumePresetByID(r.Context(), s.presetDB, req.PresetID, consumer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "consume failed: "+err.Error(), true)
		return
	}

	resp := ConsumePresetResponse{
		Consumed:  consumedAt > 0,
		LatencyMs: time.Since(start).Milliseconds(),
	}
	if consumedAt > 0 {
		resp.ConsumedAt = time.Unix(consumedAt, 0).UTC().Format(time.RFC3339)
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/consume-preset",
		params:     map[string]any{"presetId": req.PresetID, "reason": req.ConsumedReason},
		result:     "consumed:" + req.PresetID,
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}
