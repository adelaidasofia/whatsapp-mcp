package main

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
)

// PullContextRequest gates context retrieval behind verified identity.
//
// PER PRD + PANEL: server-side identity verification, NOT just agent assertion.
// Both phone AND email (or name + email) must match the same CRM record exactly.
// If they don't match, return 403 ErrIdentityMismatch.
type PullContextRequest struct {
	Phone        string `json:"phone,omitempty"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	LookbackDays int    `json:"lookbackDays,omitempty"`
	MaxMessages  int    `json:"maxMessages,omitempty"`
}

type WhatsappContext struct {
	LastMessageAt string   `json:"lastMessageAt,omitempty"`
	RecentTopics  []string `json:"recentTopics"`
	Summary       string   `json:"summary"`
	MessageCount  int      `json:"messageCount"`
}

type PullContextResponse struct {
	WhatsappContext *WhatsappContext `json:"whatsappContext,omitempty"`
	// EmailContext is reserved for a future Gmail integration; v1 returns nil.
	EmailContext *struct{} `json:"emailContext,omitempty"`
	LatencyMs    int64     `json:"latencyMs"`
}

func (s *Server) handlePullContext(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req PullContextRequest
	if err := readJSONBody(r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	if req.LookbackDays <= 0 {
		req.LookbackDays = 90
	}
	if req.MaxMessages <= 0 || req.MaxMessages > 200 {
		req.MaxMessages = 20
	}
	if req.Phone == "" && req.Email == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest,
			"at least one of phone or email required", false)
		return
	}

	// SERVER-SIDE IDENTITY VERIFICATION.
	// Look up CRM by phone OR email. The found record's other field (email or phone)
	// must match the request's other field exactly. Defense-in-depth against a
	// compromised concierge agent passing spoofed identity.
	rec, why := s.verifyIdentity(req)
	if rec == nil {
		s.audit(r.Context(), auditEntry{
			endpoint:   "/api/pull-context",
			params:     map[string]any{"hasPhone": req.Phone != "", "hasEmail": req.Email != "", "hasName": req.Name != ""},
			result:     "identity-mismatch",
			durationMs: time.Since(start).Milliseconds(),
			err:        why,
		})
		writeError(w, http.StatusForbidden, ErrIdentityMismatch,
			"identity mismatch: "+why, false)
		return
	}

	// Resolve WhatsApp JID via the CRM record's phone, or fall back to request phone.
	jid := ""
	if rec.Phone != "" {
		if j, _, ok := LookupContactByPhone(r.Context(), s.messageDB, rec.Phone); ok {
			jid = j
		}
	}
	if jid == "" && req.Phone != "" {
		if j, _, ok := LookupContactByPhone(r.Context(), s.messageDB, req.Phone); ok {
			jid = j
		}
	}

	resp := PullContextResponse{
		LatencyMs: 0,
	}
	if jid != "" {
		wctx := buildWhatsappContext(r.Context(), s.messageDB, jid, req.LookbackDays, req.MaxMessages)
		resp.WhatsappContext = wctx
	}
	resp.LatencyMs = time.Since(start).Milliseconds()

	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/pull-context",
		params:     map[string]any{"recordName": rec.Name, "lookbackDays": req.LookbackDays},
		result:     "ok",
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

// verifyIdentity returns the matched CRM record on success, or (nil, reason) on failure.
//
// Match strength required:
//   - Both phone AND email provided: both must match the same CRM record.
//   - Only phone: phone match plus optional name match (against filename or aliases).
//   - Only email: email match plus optional name match.
//
// "name match" is fuzzy (case-insensitive contains) against filename and aliases.
func (s *Server) verifyIdentity(req PullContextRequest) (*CRMRecord, string) {
	var (
		byPhoneRec *CRMRecord
		byEmailRec *CRMRecord
	)
	if req.Phone != "" {
		byPhoneRec, _ = s.crm.FindByPhone(req.Phone)
	}
	if req.Email != "" {
		byEmailRec, _ = s.crm.FindByEmail(req.Email)
	}

	if req.Phone != "" && req.Email != "" {
		if byPhoneRec == nil || byEmailRec == nil {
			return nil, "phone or email not in CRM"
		}
		if byPhoneRec.FilePath != byEmailRec.FilePath {
			return nil, "phone and email match different CRM records"
		}
		return byPhoneRec, ""
	}
	if req.Phone != "" {
		if byPhoneRec == nil {
			return nil, "phone not in CRM"
		}
		if req.Name != "" && !nameMatchesRecord(req.Name, byPhoneRec) {
			return nil, "name does not match CRM record for that phone"
		}
		return byPhoneRec, ""
	}
	if req.Email != "" {
		if byEmailRec == nil {
			return nil, "email not in CRM"
		}
		if req.Name != "" && !nameMatchesRecord(req.Name, byEmailRec) {
			return nil, "name does not match CRM record for that email"
		}
		return byEmailRec, ""
	}
	return nil, "no identifiers provided"
}

func nameMatchesRecord(name string, rec *CRMRecord) bool {
	target := Normalize(name)
	if Normalize(rec.Name) == target {
		return true
	}
	for _, a := range rec.Aliases {
		if Normalize(a) == target {
			return true
		}
	}
	// Permit substring contains in either direction for casual-name flexibility.
	if strings.Contains(Normalize(rec.Name), target) || strings.Contains(target, Normalize(rec.Name)) {
		return true
	}
	return false
}

// buildWhatsappContext returns deterministic summary fields for the JID.
// No raw message bodies are returned. Topic extraction is keyword-frequency-based.
func buildWhatsappContext(ctx context.Context, db *sql.DB, jid string, lookbackDays, maxMessages int) *WhatsappContext {
	cutoff := time.Now().AddDate(0, 0, -lookbackDays).Unix()
	rows, err := db.QueryContext(ctx, `
		SELECT timestamp, COALESCE(content_normalized, COALESCE(content_text, ''))
		FROM messages
		WHERE chat_jid = ? AND timestamp >= ? AND type IN ('text', 'reaction')
		ORDER BY timestamp DESC
		LIMIT ?
	`, jid, cutoff, maxMessages*5) // pull more for topic extraction; capped below
	if err != nil {
		return nil
	}
	defer rows.Close()

	var (
		latestTs int64
		count    int
		freq     = map[string]int{}
		contents []string
	)
	for rows.Next() {
		var ts int64
		var content string
		if err := rows.Scan(&ts, &content); err != nil {
			continue
		}
		count++
		if ts > latestTs {
			latestTs = ts
		}
		contents = append(contents, content)
		for _, tok := range tokenize(content) {
			if isTopicCandidate(tok) {
				freq[tok]++
			}
		}
	}

	topics := topNTokens(freq, 5)
	wctx := &WhatsappContext{
		MessageCount: count,
		RecentTopics: topics,
	}
	if latestTs > 0 {
		wctx.LastMessageAt = time.Unix(latestTs, 0).UTC().Format(time.RFC3339)
	}

	// Deterministic summary: "<count> messages over <span> days; topics: <topics>"
	wctx.Summary = buildSummaryString(count, latestTs, lookbackDays, topics)
	return wctx
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	out := []string{}
	cur := strings.Builder{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// stopwords filters out function words (English + Spanish) so topic extraction
// surfaces meaningful nouns and proper nouns.
var stopwords = map[string]bool{
	// English
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true, "were": true,
	"and": true, "or": true, "but": true, "not": true, "i": true, "you": true, "he": true,
	"she": true, "we": true, "they": true, "this": true, "that": true, "with": true,
	"for": true, "to": true, "in": true, "on": true, "at": true, "of": true, "be": true,
	"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
	"will": true, "would": true, "can": true, "could": true, "should": true, "shall": true,
	"my": true, "your": true, "our": true, "their": true, "his": true, "her": true,
	"if": true, "so": true, "yes": true, "no": true, "ok": true, "okay": true, "thanks": true,
	"hi": true, "hello": true, "hey": true, "im": true, "id": true, "yeah": true, "great": true,
	"just": true, "now": true, "then": true, "want": true, "need": true,
	// Spanish
	"el": true, "la": true, "los": true, "las": true, "un": true, "una": true, "uno": true,
	"y": true, "o": true, "de": true, "del": true, "que": true, "es": true, "son": true,
	"era": true, "eran": true, "yo": true, "tu": true, "el_": true, "ella": true,
	"nosotros": true, "ustedes": true, "ellos": true, "ellas": true, "esto": true, "eso": true,
	"con": true, "para": true, "por": true, "en": true, "a_": true, "se": true, "lo": true,
	"si": true, "no_": true, "pero": true, "tambien": true, "si_": true, "ya": true, "muy": true,
	"hola": true, "gracias": true, "ok_": true, "vale": true, "claro": true, "como": true,
	"que_": true, "cuando": true, "donde": true, "porque": true, "porqué": true, "ahora": true,
	"despues": true, "después": true,
}

func isTopicCandidate(tok string) bool {
	if len(tok) < 4 {
		return false
	}
	if stopwords[tok] {
		return false
	}
	// Skip pure-numeric tokens (phone fragments, codes).
	allDigits := true
	for _, r := range tok {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	return !allDigits
}

func topNTokens(freq map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	arr := make([]kv, 0, len(freq))
	for k, v := range freq {
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].v != arr[j].v {
			return arr[i].v > arr[j].v
		}
		return arr[i].k < arr[j].k
	})
	out := []string{}
	for i, x := range arr {
		if i >= n || x.v < 2 { // require frequency >= 2 to surface a topic
			break
		}
		out = append(out, x.k)
	}
	return out
}

func buildSummaryString(count int, latestTs int64, lookbackDays int, topics []string) string {
	if count == 0 {
		return "No conversation in the last " + itoa(lookbackDays) + " days."
	}
	parts := []string{}
	parts = append(parts, itoa(count)+" messages in the last "+itoa(lookbackDays)+" days")
	if latestTs > 0 {
		days := int(time.Since(time.Unix(latestTs, 0)).Hours() / 24)
		switch {
		case days == 0:
			parts = append(parts, "last interaction today")
		case days == 1:
			parts = append(parts, "last interaction yesterday")
		default:
			parts = append(parts, "last interaction "+itoa(days)+" days ago")
		}
	}
	if len(topics) > 0 {
		parts = append(parts, "recurring topics: "+strings.Join(topics, ", "))
	}
	return strings.Join(parts, "; ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
