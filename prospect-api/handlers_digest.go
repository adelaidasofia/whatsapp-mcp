package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DigestRequest is the public surface for /api/morning-digest.
type DigestRequest struct {
	Date string `json:"date,omitempty"` // YYYY-MM-DD; defaults to today
}

// DigestResponse is the response for /api/morning-digest.
type DigestResponse struct {
	DigestFilePath string `json:"digestFilePath"`
	NoteCount      int    `json:"noteCount"`
	LatencyMs      int64  `json:"latencyMs"`
}

// digestNoteEntry holds the data extracted from one inbox file for digest rendering.
type digestNoteEntry struct {
	FilePath  string
	Category  string
	Urgency   string
	Summary   string
	ArrivedAt time.Time
	Email     string
	Phone     string
}

// inboxFrontmatter is the minimal frontmatter we read from each inbox file.
type inboxFrontmatter struct {
	Category  string `yaml:"category"`
	Urgency   string `yaml:"urgency"`
	GuestEmail string `yaml:"guestEmail"`
	GuestPhone string `yaml:"guestPhone"`
	ArrivedAt string `yaml:"arrivedAt"`
}

// handleMorningDigest serves POST /api/morning-digest.
//
// Reads all inbox files whose arrivedAt falls in the previous 24 hours of the
// requested date, generates a trends section + per-note list, and writes the
// digest to <vault inbox root>/Digest/<YYYY-MM-DD>-digest.md.
// All deterministic — no LLM call.
func (s *Server) handleMorningDigest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req DigestRequest
	if err := readJSONBody(r, &req, 1024); err != nil {
		writeError(w, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON body: "+err.Error(), false)
		return
	}

	if s.cfg.VaultInboxPath == "" {
		writeError(w, http.StatusServiceUnavailable, ErrServiceUnavailab,
			"PROSPECT_VAULT_INBOX_PATH not configured", false)
		return
	}

	loc, err := time.LoadLocation(s.cfg.TimeZone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, "load tz: "+err.Error(), true)
		return
	}

	// Resolve target date.
	var targetDate time.Time
	if req.Date != "" {
		parsed, err := time.ParseInLocation("2006-01-02", req.Date, loc)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrInvalidRequest, "date must be YYYY-MM-DD: "+err.Error(), false)
			return
		}
		targetDate = parsed
	} else {
		targetDate = time.Now().In(loc)
	}

	// Window: previous 24h ending at midnight of targetDate (local).
	windowEnd := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, loc)
	windowStart := windowEnd.Add(-24 * time.Hour)

	// Also collect 7-day window for repeat-sender trend.
	sevenDayStart := windowStart.Add(-6 * 24 * time.Hour)

	var notes24h []digestNoteEntry
	var notes7d []digestNoteEntry

	err = filepath.WalkDir(s.cfg.VaultInboxPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			// Skip the Digest sub-directory.
			if d.Name() == "Digest" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		fm, summary, err := readInboxFileFrontmatter(path)
		if err != nil {
			return nil
		}

		arrivedAt, err := time.ParseInLocation(time.RFC3339, fm.ArrivedAt, loc)
		if err != nil {
			arrivedAt, err = time.Parse(time.RFC3339, fm.ArrivedAt)
			if err != nil {
				return nil
			}
		}

		entry := digestNoteEntry{
			FilePath:  path,
			Category:  fm.Category,
			Urgency:   fm.Urgency,
			Summary:   summary,
			ArrivedAt: arrivedAt,
			Email:     fm.GuestEmail,
			Phone:     fm.GuestPhone,
		}

		if !arrivedAt.Before(windowStart) && arrivedAt.Before(windowEnd) {
			notes24h = append(notes24h, entry)
		}
		if !arrivedAt.Before(sevenDayStart) && arrivedAt.Before(windowEnd) {
			notes7d = append(notes7d, entry)
		}
		return nil
	})
	if err != nil {
		log.Printf("morning-digest: walk inbox: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "walk inbox: "+err.Error(), true)
		return
	}

	// Build trends.
	topCategory := mostCommonCategory(notes24h)

	// Repeat senders in 7d (≥2 notes by same email or phone).
	repeatSenderCount := countRepeatSenders(notes7d)

	// Week-on-week delta.
	prevWindowEnd := windowStart
	prevWindowStart := prevWindowEnd.Add(-24 * time.Hour)
	var prevCount int
	for _, n := range notes7d {
		if !n.ArrivedAt.Before(prevWindowStart) && n.ArrivedAt.Before(prevWindowEnd) {
			prevCount++
		}
	}
	wowDelta := len(notes24h) - prevCount

	// Write digest.
	digestDir := filepath.Join(s.cfg.VaultInboxPath, "Digest")
	if err := os.MkdirAll(digestDir, 0o755); err != nil {
		log.Printf("morning-digest: mkdir digest dir: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "mkdir digest: "+err.Error(), true)
		return
	}
	dateStr := targetDate.Format("2006-01-02")
	digestPath := filepath.Join(digestDir, dateStr+"-digest.md")

	content := buildDigestContent(dateStr, notes24h, topCategory, repeatSenderCount, wowDelta, windowStart, windowEnd)
	if err := os.WriteFile(digestPath, []byte(content), 0o644); err != nil {
		log.Printf("morning-digest: write digest: %v", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "write digest: "+err.Error(), true)
		return
	}

	resp := DigestResponse{
		DigestFilePath: digestPath,
		NoteCount:      len(notes24h),
		LatencyMs:      time.Since(start).Milliseconds(),
	}
	s.audit(r.Context(), auditEntry{
		endpoint:   "/api/morning-digest",
		params:     map[string]any{"date": dateStr, "noteCount": len(notes24h)},
		result:     "ok",
		durationMs: resp.LatencyMs,
	})
	writeJSON(w, http.StatusOK, resp)
}

func buildDigestContent(dateStr string, notes []digestNoteEntry, topCat string, repeatSenders, wowDelta int, windowStart, windowEnd time.Time) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Morning Digest — %s\n\n", dateStr))
	b.WriteString(fmt.Sprintf("*Window: %s → %s*\n\n",
		windowStart.Format("2006-01-02 15:04 MST"),
		windowEnd.Format("2006-01-02 15:04 MST")))

	// Trends.
	b.WriteString("## Trends\n\n")
	if topCat != "" {
		b.WriteString(fmt.Sprintf("- **Top intent (24h):** %s\n", topCat))
	} else {
		b.WriteString("- **Top intent (24h):** no notes\n")
	}
	b.WriteString(fmt.Sprintf("- **Repeat senders (7d, ≥2 notes):** %d\n", repeatSenders))
	wowStr := fmt.Sprintf("%+d", wowDelta)
	b.WriteString(fmt.Sprintf("- **Week-on-week volume:** %s notes vs same weekday last week\n\n", wowStr))

	// Note list.
	b.WriteString("## Notes\n\n")
	if len(notes) == 0 {
		b.WriteString("*No inbox notes in the past 24 hours.*\n")
		return b.String()
	}

	// Sort by arrivedAt descending.
	sort.Slice(notes, func(i, j int) bool {
		return notes[i].ArrivedAt.After(notes[j].ArrivedAt)
	})

	for _, n := range notes {
		deeplink := fmt.Sprintf("[open](%s)", n.FilePath)
		b.WriteString(fmt.Sprintf("- [%s] %s: %s (%s)\n",
			strings.ToUpper(n.Urgency),
			n.Category,
			n.Summary,
			deeplink,
		))
	}
	b.WriteString("\n")
	return b.String()
}

// readInboxFileFrontmatter parses the YAML frontmatter of an inbox file and
// extracts the first H2 section body as a summary proxy.
func readInboxFileFrontmatter(path string) (*inboxFrontmatter, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	content := string(data)

	if !strings.HasPrefix(content, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter")
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, "", fmt.Errorf("unterminated frontmatter")
	}
	fmRaw := rest[:end]
	body := rest[end+5:]

	var fm inboxFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, "", err
	}

	// Extract summary: first non-empty line after "## Summary".
	summary := ""
	inSummary := false
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "## Summary" {
			inSummary = true
			continue
		}
		if inSummary {
			if strings.HasPrefix(line, "## ") {
				break
			}
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				summary = trimmed
				break
			}
		}
	}

	return &fm, summary, nil
}

func mostCommonCategory(notes []digestNoteEntry) string {
	counts := map[string]int{}
	for _, n := range notes {
		if n.Category != "" {
			counts[n.Category]++
		}
	}
	best, bestN := "", 0
	for cat, n := range counts {
		if n > bestN {
			best, bestN = cat, n
		}
	}
	return best
}

func countRepeatSenders(notes []digestNoteEntry) int {
	emailHits := map[string]int{}
	phoneHits := map[string]int{}
	for _, n := range notes {
		if n.Email != "" {
			emailHits[strings.ToLower(n.Email)]++
		}
		if n.Phone != "" {
			phoneHits[DigitsOnly(n.Phone)]++
		}
	}
	seen := map[string]bool{}
	count := 0
	for addr, n := range emailHits {
		if n >= 2 && !seen[addr] {
			seen[addr] = true
			count++
		}
	}
	for phone, n := range phoneHits {
		if n >= 2 && !seen[phone] {
			seen[phone] = true
			count++
		}
	}
	return count
}
