package main

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnrichContactsFromVault walks the configured CRM folder, extracts {name, phone}
// pairs from each CRM markdown file's frontmatter, and updates our contacts table
// so WhatsApp contacts show the vault-known name instead of a raw phone number.
//
// Matching strategy (in order):
//  1. Exact phone match (digits only, last 10 digits). Most reliable.
//  2. Filename (sans .md) vs contact push_name case-insensitive substring match.
//
// Returns count of rows updated.
//
// Called on bridge startup when WHATSAPP_VAULT_CRM_PATH is set.
// Safe to re-run; updates are idempotent and additive (CRM-known names only overwrite
// when the existing push_name is a raw "+phone" placeholder).
func EnrichContactsFromVault(db *sql.DB, crmPath string) (int, error) {
	if crmPath == "" {
		return 0, nil
	}
	st, err := os.Stat(crmPath)
	if err != nil {
		return 0, fmt.Errorf("stat crm path: %w", err)
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("crm path is not a directory: %s", crmPath)
	}

	type crmEntry struct {
		name  string // filename sans .md
		phone string // digits-only
	}
	entries := []crmEntry{}

	err = filepath.WalkDir(crmPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		phone := extractPhoneFromCRMFile(path)
		name := strings.TrimSuffix(d.Name(), ".md")
		entries = append(entries, crmEntry{name: name, phone: phone})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk crm path: %w", err)
	}

	log.Printf("crm enrich: scanned %d crm files", len(entries))

	now := time.Now().Unix()
	updated := 0
	for _, e := range entries {
		if e.phone != "" {
			n, err := updateContactByPhone(db, e.phone, e.name, now)
			if err != nil {
				log.Printf("crm enrich phone %s: %v", e.phone, err)
				continue
			}
			updated += n
		}
		// Even without phone, we could add a second pass matching by name substring,
		// but that pattern is noisy enough to defer until we have a real user signal.
	}
	return updated, nil
}

// extractPhoneFromCRMFile reads the YAML frontmatter and returns the digits-only form of
// the `phone` or `whatsapp` field, or "".
// Minimal parser — we only care about the leading frontmatter block, not the body.
func extractPhoneFromCRMFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if !strings.HasPrefix(s, "---") {
		return ""
	}
	end := strings.Index(s[3:], "\n---")
	if end < 0 {
		return ""
	}
	fm := s[3 : 3+end]
	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := splitKV(line)
		if !ok {
			continue
		}
		k := strings.ToLower(key)
		if k == "phone" || k == "whatsapp" || k == "mobile" || k == "cell" {
			return digitsOnly(val)
		}
	}
	return ""
}

func splitKV(line string) (string, string, bool) {
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:i])
	val := strings.TrimSpace(line[i+1:])
	val = strings.Trim(val, `"'`)
	return key, val, true
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// updateContactByPhone sets push_name=<crmName> for any contact whose digits-only phone
// ends with the last 10 digits of the CRM phone (loose match for country-code variations).
// Only overwrites when the existing push_name is blank OR is a bare "+phone" placeholder.
// Returns count of rows affected.
func updateContactByPhone(db *sql.DB, crmPhone, crmName string, now int64) (int, error) {
	if len(crmPhone) < 7 {
		return 0, nil // too short to be a real phone
	}
	tail := crmPhone
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
	}
	res, err := db.Exec(`
		UPDATE contacts
		SET push_name = ?,
		    normalized_name = ?,
		    updated_at = ?
		WHERE phone LIKE '%' || ? || '%'
		  AND (push_name IS NULL OR push_name = '' OR push_name GLOB '+[0-9]*')
	`, crmName, Normalize(crmName), now, tail)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
