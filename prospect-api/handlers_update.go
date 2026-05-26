package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
//
// mode (default "additive"): when "replace_primary", email and phone trigger the
// primary-rotation path — existing primary moves to the historical array
// (emails: [] / phones: []), new value becomes primary. Other fields stay
// additive regardless of mode. Codified 2026-04-27 (handoff Piece 1).
type UpdateCRMRequest struct {
	RecordID       string     `json:"recordId"`
	Updates        CRMUpdates `json:"updates"`
	ForceOverwrite bool       `json:"forceOverwrite,omitempty"`
	Mode           UpdateMode `json:"mode,omitempty"`
	// Optional. Surface to the audit log so a future operator can see who
	// the rotation was for. Not authenticated against record content; the
	// recordId itself is authoritative.
	GuestName string `json:"guestName,omitempty"`
}

type UpdateCRMResponse struct {
	Updated       bool           `json:"updated"`
	RecordID      string         `json:"recordId"`
	FieldsWritten []string       `json:"fieldsWritten"`
	Replaced      []ReplaceEvent `json:"replaced,omitempty"`
	LatencyMs     int64          `json:"latencyMs"`
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
	// Validate mode — refuse unknown values rather than silently falling
	// back to additive (would mask a typo'd request).
	switch req.Mode {
	case "", UpdateModeAdditive, UpdateModeReplacePrimary:
		// ok
	default:
		writeError(w, http.StatusBadRequest, ErrInvalidRequest,
			"invalid mode: "+string(req.Mode)+" (allowed: additive, replace_primary)", false)
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = UpdateModeAdditive
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

	written, replaced, err := s.crm.UpdateCRM(rec.FilePath, req.Updates, req.ForceOverwrite, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "crm write failed: "+err.Error(), true)
		return
	}

	// Sidecar JSONL audit log for primary rotations. Per handoff: write to
	// ~/.claude/whatsapp-mcp/prospect-api/replace-audit.log so a human can
	// `tail` it independently of the SQLite audit table.
	for _, ev := range replaced {
		appendReplaceAuditLine(replaceAuditEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			RecordID:  req.RecordID,
			GuestName: req.GuestName,
			Field:     ev.Field,
			OldValue:  ev.OldValue,
			NewValue:  ev.NewValue,
			Source:    "/api/update-crm",
		})
	}

	resp := UpdateCRMResponse{
		Updated:       len(written) > 0,
		RecordID:      req.RecordID,
		FieldsWritten: written,
		Replaced:      replaced,
		LatencyMs:     time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint: "/api/update-crm",
		params: map[string]any{
			"recordId": req.RecordID,
			"force":    req.ForceOverwrite,
			"mode":     string(mode),
			"replaced": len(replaced),
		},
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

// --- Replace audit JSONL ---------------------------------------------------

type replaceAuditEntry struct {
	Timestamp string `json:"timestamp"`
	RecordID  string `json:"recordId"`
	GuestName string `json:"guestName,omitempty"`
	Field     string `json:"field"`
	OldValue  string `json:"oldValue"`
	NewValue  string `json:"newValue"`
	Source    string `json:"source"`
}

// replaceAuditPath returns the absolute path of the sidecar JSONL log.
// We resolve it relative to the running binary's parent dir so the log
// lives next to the bridge source, regardless of the launchd cwd.
func replaceAuditPath() string {
	// $HOME/.claude/whatsapp-mcp/prospect-api/replace-audit.log
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fallback to cwd if HOME isn't set (shouldn't happen on macOS launchd).
		return "replace-audit.log"
	}
	return filepath.Join(home, ".claude", "whatsapp-mcp", "prospect-api", "replace-audit.log")
}

var replaceAuditMu sync.Mutex

func appendReplaceAuditLine(e replaceAuditEntry) {
	replaceAuditMu.Lock()
	defer replaceAuditMu.Unlock()
	path := replaceAuditPath()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		// Audit failure is non-fatal — the SQLite audit row still lands
		// via s.audit. Log to stderr so launchd captures it.
		fmt.Fprintf(os.Stderr, "replace-audit open failed: %v\n", err)
		return
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replace-audit marshal failed: %v\n", err)
		return
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "replace-audit write failed: %v\n", err)
	}
}
