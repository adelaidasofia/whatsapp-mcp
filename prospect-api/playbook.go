package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// NegotiatorSection holds the parsed terms for one playbook category.
type NegotiatorSection struct {
	Category        string
	CounterConds    []string
	FloorLanguage   string
	GracefulExit    string
	PlaybookVersion string // ISO8601 mtime of the playbook file
}

// ParseNegotiatorPlaybook reads the playbook markdown from path and returns the
// section for the requested category. Returns ErrNotFound if missing.
//
// Playbook format per PHASE-B.md spec:
//
//	## <category>
//	counter_conditions:
//	  - "string"
//	floor_language: "string"
//	graceful_exit: "string"
//	special_notes: "optional, ignored by bridge"
func ParseNegotiatorPlaybook(path, category string) (*NegotiatorSection, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat playbook: %w", err)
	}
	mtime := fi.ModTime().UTC().Format(time.RFC3339)

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open playbook: %w", err)
	}
	defer f.Close()

	var (
		inSection      bool
		inCounterConds bool
		sec            NegotiatorSection
	)
	sec.PlaybookVersion = mtime

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Detect section header.
		if strings.HasPrefix(trimmed, "## ") {
			heading := strings.TrimPrefix(trimmed, "## ")
			if inSection {
				// We were in the right section and hit the next one — done.
				break
			}
			if strings.EqualFold(heading, category) {
				inSection = true
				sec.Category = heading
			}
			continue
		}
		if !inSection {
			continue
		}

		// counter_conditions block.
		if strings.HasPrefix(trimmed, "counter_conditions:") {
			inCounterConds = true
			continue
		}
		if inCounterConds {
			if strings.HasPrefix(trimmed, "- ") {
				val := strings.TrimPrefix(trimmed, "- ")
				val = strings.Trim(val, `"`)
				sec.CounterConds = append(sec.CounterConds, val)
				continue
			}
			inCounterConds = false
		}

		// Single-line keys.
		if after, ok := cutPrefix(trimmed, "floor_language:"); ok {
			sec.FloorLanguage = strings.Trim(strings.TrimSpace(after), `"`)
			continue
		}
		if after, ok := cutPrefix(trimmed, "graceful_exit:"); ok {
			sec.GracefulExit = strings.Trim(strings.TrimSpace(after), `"`)
			continue
		}
		// special_notes is parsed but intentionally ignored.
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan playbook: %w", err)
	}
	if !inSection {
		return nil, nil // caller maps to 404 + playbook-section-missing
	}
	return &sec, nil
}

// ScaffoldNegotiatorPlaybook creates the playbook file at path with TODO
// placeholders for all five categories. Idempotent — does nothing if the file
// already exists.
func ScaffoldNegotiatorPlaybook(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	const scaffold = `# Negotiator Playbook

This file is read live on every /api/get-negotiator-terms call. Edit the
sections below to customize the concierge's negotiation behavior per category.
Do NOT rename the ## headings — the parser matches them exactly (case-insensitive).

---

## speaking_ask
counter_conditions:
  - "TODO: add counter condition"
floor_language: "TODO: floor language for speaking asks"
graceful_exit: "TODO: graceful decline language"
special_notes: ""

---

## press_media
counter_conditions:
  - "TODO: add counter condition"
floor_language: "TODO: floor language for press / media requests"
graceful_exit: "TODO: graceful decline language"
special_notes: ""

---

## partnership
counter_conditions:
  - "TODO: add counter condition"
floor_language: "TODO: floor language for partnership proposals"
graceful_exit: "TODO: graceful decline language"
special_notes: ""

---

## introduction_request
counter_conditions:
  - "TODO: add counter condition"
floor_language: "TODO: floor language for intro requests"
graceful_exit: "TODO: graceful decline language"
special_notes: ""

---

## consulting_misfit
counter_conditions:
  - "Point to the next-cheaper published tier: Personal Install ($1,500), Team Install ($3,500), Custom ($10,000), Advisory ($4,000-5,000/month)."
  - "Do NOT offer the free AI Diagnostic. Diagnostic is reserved for above-floor qualified leads only."
floor_language: "We appreciate your interest. The best fit for your situation is our Personal Install tier at $1,500, which gives you a fully built AI second-brain without a custom engagement."
graceful_exit: "If none of our published tiers fit your budget right now, I'd recommend starting with our free resources: the ai-brain-starter repo on GitHub and the Substack. When you're ready to invest in a full build, we'll be here."
special_notes: "CRITICAL: Never offer the AI Diagnostic to below-floor. Tiers are hard-coded above; do not invent custom numbers."
`
	if err := os.MkdirAll(extractDir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir playbook dir: %w", err)
	}
	return os.WriteFile(path, []byte(scaffold), 0o644)
}

// cutPrefix is strings.CutPrefix for Go < 1.20 compatibility.
func cutPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func extractDir(path string) string {
	idx := strings.LastIndexByte(path, '/')
	if idx < 0 {
		return "."
	}
	return path[:idx]
}
