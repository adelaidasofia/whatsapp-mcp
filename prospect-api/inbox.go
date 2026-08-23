package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InboxFileParams holds all data needed to write one inbox file.
type InboxFileParams struct {
	Category       string
	GuestName      string
	GuestEmail     string
	GuestPhone     string
	GuestContext   string
	OneLineSummary string
	Urgency        string
	CRMRecordID    string
	ReasoningBrief *ReasoningBrief
	NegTerms       *NegotiatorSection // populated by relay-note; drives Negotiation log section
	HighConfidence bool               // true when confidence ≥ 95 AND lean = accept
}

// ReasoningBrief is the structured brief from the relay-note request.
type ReasoningBrief struct {
	Audience string `json:"audience"`
	Fit      string `json:"fit"`
	Upside   string `json:"upside"`
	Downside string `json:"downside"`
	Lean     string `json:"lean"` // accept|decline|consider
}

// WriteInboxFile creates the inbox markdown file at the configured vault inbox
// path and returns the absolute path of the written file.
//
// Path format: <vault root>/📮 Inbox/<YYYY-MM-DD>-<HHmm>-<category>-<guest-slug>.md
// Collision: appends -<short-uuid> before .md.
func WriteInboxFile(inboxRoot string, p InboxFileParams, arrivedAt time.Time) (string, error) {
	if err := os.MkdirAll(inboxRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir inbox root: %w", err)
	}

	slug := guestSlug(p.GuestName)
	dateStr := arrivedAt.Format("2006-01-02")
	timeStr := arrivedAt.Format("1504")
	base := fmt.Sprintf("%s-%s-%s-%s.md", dateStr, timeStr, p.Category, slug)
	path := filepath.Join(inboxRoot, base)

	// Collision guard: append short UUID before .md.
	if _, err := os.Stat(path); err == nil {
		id := uuid.New().String()[:8]
		base = fmt.Sprintf("%s-%s-%s-%s-%s.md", dateStr, timeStr, p.Category, slug, id)
		path = filepath.Join(inboxRoot, base)
	}

	content := buildInboxContent(p, arrivedAt)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write inbox file: %w", err)
	}

	// Enforce path-prefix safety.
	absInbox, _ := filepath.Abs(inboxRoot)
	absPath, _ := filepath.Abs(path)
	if !strings.HasPrefix(absPath, absInbox) {
		_ = os.Remove(path)
		return "", fmt.Errorf("path traversal detected: %s is outside %s", absPath, absInbox)
	}

	return path, nil
}

func buildInboxContent(p InboxFileParams, arrivedAt time.Time) string {
	var b strings.Builder

	// Frontmatter.
	b.WriteString("---\n")
	b.WriteString("type: inbox\n")
	b.WriteString(fmt.Sprintf("category: %s\n", p.Category))
	b.WriteString(fmt.Sprintf("guestName: %s\n", yamlStr(p.GuestName)))
	if p.GuestEmail != "" {
		b.WriteString(fmt.Sprintf("guestEmail: %s\n", p.GuestEmail))
	}
	if p.GuestPhone != "" {
		b.WriteString(fmt.Sprintf("guestPhone: %s\n", p.GuestPhone))
	}
	b.WriteString(fmt.Sprintf("arrivedAt: %s\n", arrivedAt.UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("urgency: %s\n", p.Urgency))
	if p.CRMRecordID != "" {
		b.WriteString(fmt.Sprintf("crmRecordId: %s\n", p.CRMRecordID))
	}
	b.WriteString("status: unread\n")
	b.WriteString("---\n\n")

	// Body sections per PHASE-B.md spec.
	b.WriteString("## Summary\n\n")
	b.WriteString(p.OneLineSummary + "\n\n")

	b.WriteString("## Full conversation context\n\n")
	b.WriteString(p.GuestContext + "\n\n")

	if p.ReasoningBrief != nil {
		b.WriteString("## Reasoning brief\n\n")
		b.WriteString(fmt.Sprintf("1. **Who's asking:** %s\n", p.ReasoningBrief.Audience))
		b.WriteString(fmt.Sprintf("2. **The ask:** %s\n", p.OneLineSummary))
		b.WriteString(fmt.Sprintf("3. **Fit:** %s\n", p.ReasoningBrief.Fit))
		b.WriteString(fmt.Sprintf("4. **Upside / Downside:** %s / %s\n", p.ReasoningBrief.Upside, p.ReasoningBrief.Downside))
		b.WriteString(fmt.Sprintf("5. **Lean:** %s\n\n", p.ReasoningBrief.Lean))
	}

	// Suggested response: only for high-confidence accept cases.
	if p.HighConfidence && p.ReasoningBrief != nil && p.ReasoningBrief.Lean == "accept" {
		b.WriteString("## Suggested response\n\n")
		b.WriteString("*Auto-generated from high-confidence CRM match + lean: accept.*\n\n")
		if p.NegTerms != nil && p.NegTerms.FloorLanguage != "" {
			b.WriteString(p.NegTerms.FloorLanguage + "\n\n")
		}
	}

	// Negotiation log: present for negotiable categories.
	if isNegotiableCategory(p.Category) {
		b.WriteString("## Negotiation log\n\n")
		if p.NegTerms != nil && len(p.NegTerms.CounterConds) > 0 {
			b.WriteString("**Counter conditions:**\n")
			for _, c := range p.NegTerms.CounterConds {
				b.WriteString(fmt.Sprintf("- %s\n", c))
			}
			b.WriteString("\n")
			if p.NegTerms.GracefulExit != "" {
				b.WriteString(fmt.Sprintf("**Graceful exit:** %s\n\n", p.NegTerms.GracefulExit))
			}
		}
		b.WriteString("*Concierge appends asker response and final disposition here.*\n")
	}

	return b.String()
}

// isNegotiableCategory returns true for the five categories the Negotiator
// Playbook covers. relay-note populates the Negotiation log section for these.
func isNegotiableCategory(cat string) bool {
	return validNegotiatorCategories[cat]
}

// guestSlug converts a guest name to a URL-safe slug for the filename.
func guestSlug(name string) string {
	if name == "" {
		return "unknown"
	}
	s := strings.ToLower(name)
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, s)
	// Collapse multiple dashes.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// yamlStr quotes a string value for YAML frontmatter when it contains special chars.
func yamlStr(s string) string {
	if strings.ContainsAny(s, `:'"#&*?|<>[]{}`) {
		return fmt.Sprintf("%q", s)
	}
	return s
}
