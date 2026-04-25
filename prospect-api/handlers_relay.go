package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// RelayNoteRequest is the public surface for /api/relay-note.
type RelayNoteRequest struct {
	Category       string          `json:"category"`
	GuestName      string          `json:"guestName"`
	GuestEmail     string          `json:"guestEmail,omitempty"`
	GuestPhone     string          `json:"guestPhone,omitempty"`
	GuestContext   string          `json:"guestContext"`
	OneLineSummary string          `json:"oneLineSummary"`
	Urgency        string          `json:"urgency"`
	ReasoningBrief *ReasoningBrief `json:"reasoningBrief,omitempty"`
}

// RelayNoteResponse is the response for /api/relay-note.
type RelayNoteResponse struct {
	InboxFilePath  string `json:"inboxFilePath"`
	CRMUpdated     bool   `json:"crmUpdated"`
	WhatsAppPinged bool   `json:"whatsappPinged"`
	LatencyMs      int64  `json:"latencyMs"`
}

// handleRelayNote serves POST /api/relay-note.
//
// Writes an inbox file, optionally updates the CRM (whitelist-enforced),
// and calls notify-adelaida for normal/high urgency (subject to the shared
// WhatsApp ping budget).
func (s *Server) handleRelayNote(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req RelayNoteRequest
	if err := readJSONBody(r, &req, 16384); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}

	req.GuestName = strings.TrimSpace(req.GuestName)
	req.GuestContext = strings.TrimSpace(req.GuestContext)
	req.OneLineSummary = strings.TrimSpace(req.OneLineSummary)

	if req.Category == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "category is required", false)
		return
	}
	if req.GuestName == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "guestName is required", false)
		return
	}
	if req.GuestContext == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "guestContext is required", false)
		return
	}
	if req.OneLineSummary == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "oneLineSummary is required", false)
		return
	}
	if req.Urgency != "low" && req.Urgency != "normal" && req.Urgency != "high" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "urgency must be low, normal, or high", false)
		return
	}

	if s.cfg.VaultInboxPath == "" {
		writeError(w, http.StatusServiceUnavailable, ErrServiceUnavailab,
			"PROSPECT_VAULT_INBOX_PATH not configured", false)
		return
	}

	// Fetch negotiator terms if this is a negotiable category.
	var negTerms *NegotiatorSection
	if isNegotiableCategory(req.Category) && s.cfg.NegotiatorPlaybookPath != "" {
		var err error
		negTerms, err = ParseNegotiatorPlaybook(s.cfg.NegotiatorPlaybookPath, req.Category)
		if err != nil {
			log.Printf("relay-note: parse playbook: %v", err)
		}
	}

	// Resolve CRM record for confidence + CRM update.
	var crmRecord *CRMRecord
	var confidence int
	if req.GuestEmail != "" {
		if rec, _ := s.crm.FindByEmail(req.GuestEmail); rec != nil {
			crmRecord = rec
			confidence = 99
		}
	}
	if crmRecord == nil && req.GuestPhone != "" {
		if rec, _ := s.crm.FindByPhone(req.GuestPhone); rec != nil {
			crmRecord = rec
			confidence = 95
		}
	}
	if crmRecord == nil {
		if rec, _ := s.crm.FindByName(req.GuestName); rec != nil {
			crmRecord = rec
			confidence = 60
		}
	}

	highConf := confidence >= 95 &&
		req.ReasoningBrief != nil &&
		req.ReasoningBrief.Lean == "accept"

	crmRecordID := ""
	if crmRecord != nil {
		crmRecordID = crmRecord.Name
	}

	// Write the inbox file.
	params := InboxFileParams{
		Category:       req.Category,
		GuestName:      req.GuestName,
		GuestEmail:     req.GuestEmail,
		GuestPhone:     req.GuestPhone,
		GuestContext:   req.GuestContext,
		OneLineSummary: req.OneLineSummary,
		Urgency:        req.Urgency,
		CRMRecordID:    crmRecordID,
		ReasoningBrief: req.ReasoningBrief,
		NegTerms:       negTerms,
		HighConfidence: highConf,
	}
	inboxPath, err := WriteInboxFile(s.cfg.VaultInboxPath, params, time.Now())
	if err != nil {
		log.Printf("relay-note: write inbox file: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "inbox write failed: "+err.Error(), true)
		return
	}

	// CRM update (whitelist-enforced, same path as /api/update-crm).
	crmUpdated := false
	if crmRecord != nil && req.GuestContext != "" {
		updates := CRMUpdates{}
		if req.GuestEmail != "" {
			e := req.GuestEmail
			updates.Email = &e
		}
		if req.GuestPhone != "" {
			p := req.GuestPhone
			updates.Phone = &p
		}
		written, err := s.crm.UpdateCRM(crmRecord.FilePath, updates, false)
		if err != nil {
			log.Printf("relay-note: crm update: %v", err)
		} else {
			crmUpdated = len(written) > 0
		}
	}

	// Notify for normal/high urgency via notify-adelaida internals.
	waPinged := false
	if req.Urgency == "normal" || req.Urgency == "high" {
		pingMsg := fmt.Sprintf("[%s] %s — %s", strings.ToUpper(req.Urgency), req.GuestName, req.OneLineSummary)
		if len(pingMsg) > 500 {
			pingMsg = pingMsg[:497] + "..."
		}

		qh, err := newQuietHoursChecker(s.cfg)
		if err == nil && qh.ShouldSendNow(req.Urgency, time.Now()) {
			budget := &waPingBudget{db: s.presetDB, limitPH: s.cfg.WhatsAppPingsPerHour}
			ok, err := budget.CheckAndRecordPing("/api/relay-note", req.Urgency)
			if err != nil {
				log.Printf("relay-note: budget check: %v", err)
			} else if ok {
				if _, err := s.sendViaBridge(pingMsg); err != nil {
					log.Printf("relay-note: bridge ping: %v", err)
				} else {
					waPinged = true
				}
			} else {
				// Budget exhausted — queue.
				deliverAt := time.Now().Add(time.Hour).Truncate(time.Hour)
				_ = EnqueuePing(s.presetDB, pingMsg, req.Urgency, inboxPath, deliverAt)
			}
		} else if err == nil {
			// Quiet hours — queue to wake time.
			deliverAt := qh.NextWakeTime(time.Now())
			_ = EnqueuePing(s.presetDB, pingMsg, req.Urgency, inboxPath, deliverAt)
		}
	}

	resp := RelayNoteResponse{
		InboxFilePath:  inboxPath,
		CRMUpdated:     crmUpdated,
		WhatsAppPinged: waPinged,
		LatencyMs:      time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/relay-note",
		params:     map[string]any{"category": req.Category, "urgency": req.Urgency, "hasCRM": crmRecord != nil},
		result:     fmt.Sprintf("inbox=%s crmUpdated=%v waPinged=%v", inboxPath, crmUpdated, waPinged),
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}
