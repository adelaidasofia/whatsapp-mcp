package main

import (
	"net/http"
	"time"
)

// UpdateCRMRequest is the public surface for /api/update-crm.
//
// Whitelist enforced server-side. Any field not in the whitelist is dropped and logged.
// recordId is the CRM filename without .md (vault convention). The agent must already
// know the recordId from a prior /api/lookup-prospect or /api/pull-context response.
//
// forceOverwrite (default false): when false, only blank fields are filled. When true,
// existing values are replaced. Default-safe per design panel.
type UpdateCRMRequest struct {
	RecordID       string     `json:"recordId"`
	Updates        CRMUpdates `json:"updates"`
	ForceOverwrite bool       `json:"forceOverwrite,omitempty"`
}

type UpdateCRMResponse struct {
	Updated       bool     `json:"updated"`
	RecordID      string   `json:"recordId"`
	FieldsWritten []string `json:"fieldsWritten"`
	LatencyMs     int64    `json:"latencyMs"`
}

func (s *Server) handleUpdateCRM(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req UpdateCRMRequest
	if err := readJSONBody(r, &req, 8192); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.RecordID == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "recordId required", false)
		return
	}

	rec, err := s.crm.FindByName(req.RecordID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "crm read failed: "+err.Error(), true)
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, ErrNotFound, "crm record not found: "+req.RecordID, false)
		return
	}

	written, err := s.crm.UpdateCRM(rec.FilePath, req.Updates, req.ForceOverwrite)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "crm write failed: "+err.Error(), true)
		return
	}

	resp := UpdateCRMResponse{
		Updated:       len(written) > 0,
		RecordID:      req.RecordID,
		FieldsWritten: written,
		LatencyMs:     time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/update-crm",
		params:     map[string]any{"recordId": req.RecordID, "force": req.ForceOverwrite},
		result:     joinFields(written),
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

func joinFields(fields []string) string {
	if len(fields) == 0 {
		return "no-op"
	}
	out := "wrote: "
	for i, f := range fields {
		if i > 0 {
			out += ","
		}
		out += f
	}
	return out
}
