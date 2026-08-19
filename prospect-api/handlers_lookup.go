package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// LookupRequest is the public surface for /api/lookup-prospect.
//
// At least one of (name, email, phone) must be present. Multiple may be combined
// for stronger confidence; results are sorted desc by confidence and capped at 5.
type LookupRequest struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

// LookupMatch is one hit. The schema deliberately excludes lastTopic / notesPublic /
// raw message bodies. Categorization is bounded to the Category enum.
type LookupMatch struct {
	Confidence int          `json:"confidence"` // 0..100
	Record     LookupRecord `json:"record"`
}

type LookupRecord struct {
	Name         string   `json:"name"`
	Email        string   `json:"email,omitempty"`
	Phone        string   `json:"phone,omitempty"`
	Company      string   `json:"company,omitempty"`
	Role         string   `json:"role,omitempty"`
	Relationship string   `json:"relationship,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	CategoryEnum Category `json:"categoryEnum,omitempty"`
}

type LookupResponse struct {
	Matches   []LookupMatch `json:"matches"`
	LatencyMs int64         `json:"latencyMs"`
}

// confidenceFor returns the rule-driven confidence given which fields matched.
// Schema:
//
//	exact email      99
//	exact phone      95
//	name + company   85
//	name + WA contact 80
//	exact name       60
//	fuzzy name       40
type matchKind int

const (
	matchExactEmail matchKind = iota
	matchExactPhone
	matchNameCompany
	matchNameWA
	matchExactName
	matchFuzzyName
)

func confidenceForKind(k matchKind) int {
	switch k {
	case matchExactEmail:
		return 99
	case matchExactPhone:
		return 95
	case matchNameCompany:
		return 85
	case matchNameWA:
		return 80
	case matchExactName:
		return 60
	default:
		return 40
	}
}

func (s *Server) handleLookupProspect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req LookupRequest
	if err := readJSONBody(r, &req, 4096); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	req.Phone = strings.TrimSpace(req.Phone)
	if req.Name == "" && req.Email == "" && req.Phone == "" {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "at least one of name, email, phone is required", false)
		return
	}

	// Anti-enumeration: per-phone rate limit.
	if req.Phone != "" {
		if !s.phoneLimiter.Allow(DigitsOnly(req.Phone)) {
			writeError(w, http.StatusTooManyRequests, ErrRateLimited,
				"per-phone lookup rate limit exceeded", true)
			return
		}
	}

	matches := s.collectLookupMatches(r, req)

	// Sort desc by confidence, cap to 5.
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Confidence > matches[j].Confidence
	})
	if len(matches) > 5 {
		matches = matches[:5]
	}

	resp := LookupResponse{
		Matches:   matches,
		LatencyMs: time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/lookup-prospect",
		params:     map[string]any{"hasName": req.Name != "", "hasEmail": req.Email != "", "hasPhone": req.Phone != ""},
		result:     resultSummary(len(matches)),
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) collectLookupMatches(r *http.Request, req LookupRequest) []LookupMatch {
	out := []LookupMatch{}
	seenPaths := map[string]bool{}

	add := func(rec *CRMRecord, kind matchKind) {
		if rec == nil {
			return
		}
		if seenPaths[rec.FilePath] {
			return
		}
		seenPaths[rec.FilePath] = true

		// Resolve category via the deterministic categorizer.
		jid, _, _ := LookupContactByPhone(r.Context(), s.messageDB, rec.Phone)
		if jid == "" && req.Phone != "" {
			jid, _, _ = LookupContactByPhone(r.Context(), s.messageDB, req.Phone)
		}
		cat := CategorizeContact(r.Context(), s.messageDB, jid, rec)

		out = append(out, LookupMatch{
			Confidence: confidenceForKind(kind),
			Record: LookupRecord{
				Name:         rec.Name,
				Email:        rec.Email,
				Phone:        rec.Phone,
				Company:      rec.Company,
				Role:         rec.Role,
				Relationship: rec.Relationship,
				Tags:         rec.Tags,
				CategoryEnum: cat,
			},
		})
	}

	if req.Email != "" {
		if rec, _ := s.crm.FindByEmail(req.Email); rec != nil {
			add(rec, matchExactEmail)
		}
	}
	if req.Phone != "" {
		if rec, _ := s.crm.FindByPhone(req.Phone); rec != nil {
			add(rec, matchExactPhone)
		} else if jid, pushName, found := LookupContactByPhone(r.Context(), s.messageDB, req.Phone); found {
			// WhatsApp-only contact (no CRM record). Lower confidence, no CRM fields.
			cat := CategorizeContact(r.Context(), s.messageDB, jid, nil)
			out = append(out, LookupMatch{
				Confidence: 70, // between WA-only and CRM phone match
				Record: LookupRecord{
					Name:         pushName,
					Phone:        req.Phone,
					CategoryEnum: cat,
				},
			})
		}
	}
	if req.Name != "" {
		if rec, _ := s.crm.FindByName(req.Name); rec != nil {
			kind := matchExactName
			if req.Email != "" || req.Phone != "" {
				// Already covered; skip unless rec is fresh.
				if seenPaths[rec.FilePath] {
					goto fuzzyName
				}
			}
			if req.Email != "" && strings.EqualFold(rec.Company, req.Email) {
				kind = matchNameCompany
			}
			add(rec, kind)
		}
	fuzzyName:
		// WhatsApp name fuzzy match (no CRM).
		if jid, pushName, found := LookupContactByName(r.Context(), s.messageDB, req.Name); found {
			alreadyCovered := false
			for _, m := range out {
				if m.Record.Phone != "" && m.Record.Name == pushName {
					alreadyCovered = true
					break
				}
			}
			if !alreadyCovered {
				cat := CategorizeContact(r.Context(), s.messageDB, jid, nil)
				out = append(out, LookupMatch{
					Confidence: confidenceForKind(matchNameWA),
					Record: LookupRecord{
						Name:         pushName,
						CategoryEnum: cat,
					},
				})
			}
		}
	}
	return out
}

func resultSummary(count int) string {
	if count == 0 {
		return "no-match"
	}
	return "match"
}
