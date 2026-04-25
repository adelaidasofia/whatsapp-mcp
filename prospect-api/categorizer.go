package main

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Category is the tightly-controlled enum returned by /api/lookup-prospect.
// Per Howard Marks / Jackie Kennedy panel: existence + category, never content.
type Category string

const (
	CategoryBusinessDiscussion Category = "business_discussion"
	CategoryScheduledFollowup  Category = "scheduled_followup"
	CategorySocial             Category = "social"
	CategoryIntroConversation  Category = "intro_conversation"
	CategoryUnknown            Category = "unknown"
)

// CategorizeContact returns the category for a given WhatsApp contact JID
// based on:
//   1. CRM relationship/tags hints (highest signal)
//   2. Recent message scheduling-keyword scan
//   3. Recent message volume (low → intro_conversation)
//
// Pure deterministic. No LLM. No external calls.
func CategorizeContact(ctx context.Context, db *sql.DB, jid string, crm *CRMRecord) Category {
	// 1. CRM hints first. Normalize the relationship value so "close-friend",
	//    "close_friend", "close friend", "Close Friend" all match the same key.
	if crm != nil {
		rel := normalizeRelationship(crm.Relationship)
		if cat, ok := relationshipToCategory[rel]; ok {
			return cat
		}
		for _, t := range crm.Tags {
			low := normalizeRelationship(t)
			if cat, ok := relationshipToCategory[low]; ok {
				return cat
			}
		}
	}

	// 2. Scheduling-keyword scan over last 14 days.
	if jid != "" {
		hasScheduling, err := hasRecentSchedulingKeywords(ctx, db, jid, 14)
		if err == nil && hasScheduling {
			return CategoryScheduledFollowup
		}
	}

	// 3. Message volume.
	if jid != "" {
		count, _ := messageCountForJID(ctx, db, jid)
		if count == 0 {
			return CategoryUnknown
		}
		if count < 5 {
			return CategoryIntroConversation
		}
		// Default: if we have substantial conversation but no clear hint, call it business.
		// This is a conservative default; the LLM-driven concierge layer will refine in practice.
		return CategoryBusinessDiscussion
	}

	return CategoryUnknown
}

// relationshipToCategory maps normalized CRM relationship/tag values to a category.
// Synonyms are expanded so vault frontmatter can use whichever form feels natural
// ("close-friend", "close friend", "close_friend" all hit the same bucket).
var relationshipToCategory = map[string]Category{
	// Social / personal
	"close friend": CategorySocial,
	"friend":       CategorySocial,
	"family":       CategorySocial,
	"personal":     CategorySocial,
	"social":       CategorySocial,
	"sister":       CategorySocial,
	"brother":      CategorySocial,
	"mother":       CategorySocial,
	"father":       CategorySocial,
	"partner":      CategorySocial,
	"date":         CategorySocial,
	// Business / professional
	"client":       CategoryBusinessDiscussion,
	"prospect":     CategoryBusinessDiscussion,
	"advisor":      CategoryBusinessDiscussion,
	"investor":     CategoryBusinessDiscussion,
	"vendor":       CategoryBusinessDiscussion,
	"professional": CategoryBusinessDiscussion,
	"cofounder":    CategoryBusinessDiscussion,
	"co founder":   CategoryBusinessDiscussion,
	"team":         CategoryBusinessDiscussion,
	"colleague":    CategoryBusinessDiscussion,
	"contractor":   CategoryBusinessDiscussion,
	"customer":     CategoryBusinessDiscussion,
	"partner_pro":  CategoryBusinessDiscussion, // unlikely; reserved
	"acquaintance": CategoryBusinessDiscussion,
}

// normalizeRelationship lowercases, replaces hyphens/underscores with spaces,
// collapses internal whitespace, and trims. The result is keyable in
// relationshipToCategory above.
func normalizeRelationship(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	// Collapse multiple spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// schedulingKeywords are matched case-insensitively against message content text.
// Bilingual (English + Spanish) since the user's primary register is bilingual.
var schedulingKeywords = []string{
	"meet", "meeting", "schedule", "scheduled", "calendar", "call", "zoom",
	"available", "availability", "tomorrow", "next week", "monday", "tuesday",
	"wednesday", "thursday", "friday", "weekend", "today",
	// Spanish
	"reunir", "reunión", "agendar", "agenda", "llamada", "disponib", "calendario",
	"mañana", "próxima semana", "lunes", "martes", "miércoles", "jueves", "viernes",
	"fin de semana", "hoy",
}

func hasRecentSchedulingKeywords(ctx context.Context, db *sql.DB, jid string, days int) (bool, error) {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(content_normalized, COALESCE(content_text, ''))
		FROM messages
		WHERE chat_jid = ? AND timestamp >= ? AND type IN ('text', 'reaction')
		ORDER BY timestamp DESC
		LIMIT 200
	`, jid, cutoff)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		low := strings.ToLower(content)
		for _, kw := range schedulingKeywords {
			if strings.Contains(low, kw) {
				return true, nil
			}
		}
	}
	return false, nil
}

func messageCountForJID(ctx context.Context, db *sql.DB, jid string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, jid,
	).Scan(&n)
	return n, err
}

// LookupContactByPhone returns minimal whatsapp contact details for the lookup endpoint.
// Returns (nil, nil) when no whatsapp contact matches.
func LookupContactByPhone(ctx context.Context, db *sql.DB, phone string) (jid string, pushName string, found bool) {
	digits := DigitsOnly(phone)
	if len(digits) < 7 {
		return "", "", false
	}
	tail := digits
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
	}
	row := db.QueryRowContext(ctx, `
		SELECT jid, COALESCE(push_name, ''), COALESCE(verified_name, '')
		FROM contacts
		WHERE phone LIKE '%' || ? || '%'
		ORDER BY updated_at DESC
		LIMIT 1
	`, tail)
	var verName string
	if err := row.Scan(&jid, &pushName, &verName); err != nil {
		return "", "", false
	}
	if pushName == "" {
		pushName = verName
	}
	return jid, pushName, true
}

// LookupContactByName returns the most recently-updated contact whose normalized
// push_name contains the query. Used for low-confidence name-only matching.
func LookupContactByName(ctx context.Context, db *sql.DB, name string) (jid string, pushName string, found bool) {
	q := Normalize(name)
	if q == "" {
		return "", "", false
	}
	row := db.QueryRowContext(ctx, `
		SELECT jid, COALESCE(push_name, '')
		FROM contacts
		WHERE normalized_name LIKE '%' || ? || '%'
		ORDER BY updated_at DESC
		LIMIT 1
	`, q)
	if err := row.Scan(&jid, &pushName); err != nil {
		return "", "", false
	}
	return jid, pushName, true
}
