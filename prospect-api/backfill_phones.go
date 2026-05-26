package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunBackfillPhones cross-references WhatsApp contacts (which have phones) against
// vault CRM notes (which mostly do not) and writes the matched phone back to the
// CRM file's YAML frontmatter when:
//
//   1. The CRM record has no existing phone, AND
//   2. The WhatsApp contact's push_name (or verified_name) matches the CRM filename
//      OR an entry in the CRM aliases array. Match is normalized (NFD, lowercase, ASCII-fold).
//
// Default mode is dry-run: prints what WOULD be written, makes zero changes. Pass
// --apply to write. Pass --include-weak to include substring fuzzy matches (off by
// default because false positives are real and ship the wrong phone to the wrong note).
//
// Multiple WA contacts matching the same CRM file are reported as conflicts and skipped;
// the user resolves them manually.
type backfillMode struct {
	DryRun      bool
	IncludeWeak bool
}

type matchKindBackfill string

const (
	matchExactFilename matchKindBackfill = "exact-filename"
	matchExactAlias    matchKindBackfill = "exact-alias"
	matchSubstring     matchKindBackfill = "substring"
)

type backfillCandidate struct {
	CRMFilePath  string
	CRMName      string
	WAJID        string
	WAPushName   string
	WAPhone      string
	MatchKind    matchKindBackfill
	ExistingPhone string
}

func RunBackfillPhones(ctx context.Context, cfg *Config, messageDB *sql.DB, crm *CRMStore, mode backfillMode) error {
	// Load all CRM records.
	crmRecs := []*CRMRecord{}
	err := filepath.WalkDir(crm.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rec, err := readCRMFile(path)
		if err != nil {
			return nil
		}
		crmRecs = append(crmRecs, rec)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk crm: %w", err)
	}
	log.Printf("backfill: loaded %d CRM notes", len(crmRecs))

	// Build name lookup: normalized name -> []*CRMRecord (multiple possible).
	type crmIndex struct {
		byNormName  map[string][]*CRMRecord
		byNormAlias map[string][]*CRMRecord
	}
	idx := crmIndex{
		byNormName:  map[string][]*CRMRecord{},
		byNormAlias: map[string][]*CRMRecord{},
	}
	for _, r := range crmRecs {
		nm := Normalize(r.Name)
		if nm != "" {
			idx.byNormName[nm] = append(idx.byNormName[nm], r)
		}
		for _, a := range r.Aliases {
			na := Normalize(a)
			if na != "" {
				idx.byNormAlias[na] = append(idx.byNormAlias[na], r)
			}
		}
	}

	// Pull all WhatsApp contacts that have a phone AND a push_name/verified_name.
	rows, err := messageDB.QueryContext(ctx, `
		SELECT jid, COALESCE(push_name, ''), COALESCE(verified_name, ''), COALESCE(phone, '')
		FROM contacts
		WHERE COALESCE(phone, '') != ''
		  AND (COALESCE(push_name, '') != '' OR COALESCE(verified_name, '') != '')
	`)
	if err != nil {
		return fmt.Errorf("query contacts: %w", err)
	}
	defer rows.Close()

	type waRow struct {
		JID, PushName, VerifiedName, Phone string
	}
	waContacts := []waRow{}
	for rows.Next() {
		var w waRow
		if err := rows.Scan(&w.JID, &w.PushName, &w.VerifiedName, &w.Phone); err != nil {
			continue
		}
		waContacts = append(waContacts, w)
	}
	log.Printf("backfill: loaded %d WhatsApp contacts with phones", len(waContacts))

	// Match.
	type fileMatches struct {
		strong []backfillCandidate
		weak   []backfillCandidate
	}
	perFile := map[string]*fileMatches{}

	add := func(c backfillCandidate, weak bool) {
		fm := perFile[c.CRMFilePath]
		if fm == nil {
			fm = &fileMatches{}
			perFile[c.CRMFilePath] = fm
		}
		if weak {
			fm.weak = append(fm.weak, c)
		} else {
			fm.strong = append(fm.strong, c)
		}
	}

	for _, w := range waContacts {
		nameTries := []string{}
		if w.PushName != "" {
			nameTries = append(nameTries, w.PushName)
		}
		if w.VerifiedName != "" && w.VerifiedName != w.PushName {
			nameTries = append(nameTries, w.VerifiedName)
		}

		matchedExact := false
		for _, name := range nameTries {
			n := Normalize(name)
			if n == "" {
				continue
			}
			if recs, ok := idx.byNormName[n]; ok {
				for _, r := range recs {
					add(backfillCandidate{
						CRMFilePath:   r.FilePath,
						CRMName:       r.Name,
						WAJID:         w.JID,
						WAPushName:    name,
						WAPhone:       w.Phone,
						MatchKind:     matchExactFilename,
						ExistingPhone: r.Phone,
					}, false)
					matchedExact = true
				}
			}
			if recs, ok := idx.byNormAlias[n]; ok {
				for _, r := range recs {
					add(backfillCandidate{
						CRMFilePath:   r.FilePath,
						CRMName:       r.Name,
						WAJID:         w.JID,
						WAPushName:    name,
						WAPhone:       w.Phone,
						MatchKind:     matchExactAlias,
						ExistingPhone: r.Phone,
					}, false)
					matchedExact = true
				}
			}
		}
		if !matchedExact && mode.IncludeWeak {
			// Weak: substring match (push_name within filename or vice versa).
			for _, name := range nameTries {
				n := Normalize(name)
				if len(n) < 4 {
					continue
				}
				for _, r := range crmRecs {
					rn := Normalize(r.Name)
					if rn == "" {
						continue
					}
					if strings.Contains(rn, n) || strings.Contains(n, rn) {
						add(backfillCandidate{
							CRMFilePath:   r.FilePath,
							CRMName:       r.Name,
							WAJID:         w.JID,
							WAPushName:    name,
							WAPhone:       w.Phone,
							MatchKind:     matchSubstring,
							ExistingPhone: r.Phone,
						}, true)
					}
				}
			}
		}
	}

	// Walk per-file matches and decide what to write.
	type plan struct {
		c        backfillCandidate
		conflict bool
		skipped  string
	}
	plans := []plan{}
	for path, fm := range perFile {
		// Pull representative CRMRecord to inspect existing phone.
		// (All entries in fm.strong/weak share the same CRMFilePath.)
		var rep *backfillCandidate
		switch {
		case len(fm.strong) > 0:
			c := fm.strong[0]
			rep = &c
		case mode.IncludeWeak && len(fm.weak) > 0:
			c := fm.weak[0]
			rep = &c
		default:
			continue
		}
		// Skip if CRM already has a phone.
		if rep.ExistingPhone != "" {
			plans = append(plans, plan{c: *rep, skipped: "crm-already-has-phone"})
			continue
		}
		// Conflict detection: multiple WhatsApp contacts mapped to same CRM file with
		// DIFFERENT phones. We refuse to guess.
		uniquePhones := map[string]bool{}
		for _, c := range fm.strong {
			uniquePhones[c.WAPhone] = true
		}
		if mode.IncludeWeak {
			for _, c := range fm.weak {
				uniquePhones[c.WAPhone] = true
			}
		}
		if len(uniquePhones) > 1 {
			plans = append(plans, plan{c: *rep, conflict: true, skipped: "multiple-different-phones"})
			continue
		}
		_ = path
		plans = append(plans, plan{c: *rep})
	}

	// Sort plans for stable output: actionable first, then skipped.
	sort.Slice(plans, func(i, j int) bool {
		ai := plans[i].c.CRMName
		aj := plans[j].c.CRMName
		// Actionables first.
		ip := plans[i].skipped == "" && !plans[i].conflict
		jp := plans[j].skipped == "" && !plans[j].conflict
		if ip != jp {
			return ip
		}
		return ai < aj
	})

	// Print + (optionally) apply.
	actionable := 0
	skipped := 0
	conflicts := 0
	applied := 0
	fmt.Println()
	fmt.Println("=== backfill plan ===")
	for _, p := range plans {
		if p.conflict {
			conflicts++
			fmt.Printf("CONFLICT %-30s waPushName=%q (multiple different phones found; skipping)\n",
				p.c.CRMName, p.c.WAPushName)
			continue
		}
		if p.skipped != "" {
			skipped++
			fmt.Printf("SKIP     %-30s reason=%s existingPhone=%s\n",
				p.c.CRMName, p.skipped, p.c.ExistingPhone)
			continue
		}
		actionable++
		fmt.Printf("WRITE    %-30s match=%-15s waPushName=%-30q phone=%s\n",
			p.c.CRMName, p.c.MatchKind, p.c.WAPushName, p.c.WAPhone)
		if !mode.DryRun {
			applied += applyOnePhone(crm, p.c)
		}
	}

	fmt.Println()
	fmt.Println("=== backfill summary ===")
	fmt.Printf("CRM notes: %d\n", len(crmRecs))
	fmt.Printf("WhatsApp contacts (with phone): %d\n", len(waContacts))
	fmt.Printf("Actionable matches: %d\n", actionable)
	fmt.Printf("Conflicts (multiple phones for same name): %d\n", conflicts)
	fmt.Printf("Skipped (already has phone): %d\n", skipped)
	if mode.DryRun {
		fmt.Println()
		fmt.Println("DRY-RUN: no files written. Re-run with --apply to write phones to CRM frontmatter.")
	} else {
		fmt.Printf("Applied: %d files updated.\n", applied)
	}
	return nil
}

// applyOnePhone writes a single phone value to the CRM file's frontmatter via the
// existing UpdateCRM whitelist path. Returns 1 on success, 0 on no-op.
func applyOnePhone(crm *CRMStore, c backfillCandidate) int {
	// Make sure the file still exists and is readable.
	if _, err := os.Stat(c.CRMFilePath); err != nil {
		log.Printf("apply: stat %s: %v", c.CRMFilePath, err)
		return 0
	}
	phone := strings.TrimSpace(c.WAPhone)
	if phone == "" {
		return 0
	}
	if !strings.HasPrefix(phone, "+") {
		// E.164 normalize: stored phones are digits only in messages; prepend + for CRM.
		phone = "+" + phone
	}
	written, _, err := crm.UpdateCRM(c.CRMFilePath, CRMUpdates{Phone: &phone}, false, UpdateModeAdditive)
	if err != nil {
		log.Printf("apply: update %s: %v", c.CRMFilePath, err)
		return 0
	}
	if len(written) == 0 {
		return 0
	}
	return 1
}
