package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MacPerson is a single contact card from macOS Contacts.app, with all of
// their phone numbers grouped under one stable identity (ZUNIQUEID).
type MacPerson struct {
	UUID  string   // ZUNIQUEID — stable across the address book DB lifetime
	Name  string   // best display name (FIRST + LAST → FIRST → LAST → NICKNAME → ORG)
	Phones []string // unique E.164-with-leading-+ phone numbers attached to this person
}

// LoadMacContacts reads every AddressBook source DB and returns:
//   - tailToPerson: phone-tail (last 10 digits) → *MacPerson  (so a WhatsApp
//     contact lookup by phone resolves to the *whole person*, including their
//     other phones).
//   - personIndex:  ZUNIQUEID → *MacPerson  (canonical person store).
//
// Contacts are partitioned into one DB per data source (iCloud, Google,
// Exchange, …); we union all of them. Each person can have any number of
// phones, so one MacPerson may appear under several keys in tailToPerson.
func LoadMacContacts() (tailToPerson map[string]*MacPerson, personIndex map[string]*MacPerson, totalRows int, err error) {
	home, herr := os.UserHomeDir()
	if herr != nil {
		return nil, nil, 0, fmt.Errorf("home dir: %w", herr)
	}
	pattern := filepath.Join(home, "Library", "Application Support",
		"AddressBook", "Sources", "*", "AddressBook-v22.abcddb")
	dbs, gerr := filepath.Glob(pattern)
	if gerr != nil {
		return nil, nil, 0, fmt.Errorf("glob addressbook sources: %w", gerr)
	}

	tailToPerson = map[string]*MacPerson{}
	personIndex = map[string]*MacPerson{}
	if len(dbs) == 0 {
		return tailToPerson, personIndex, 0, nil
	}

	for _, dbPath := range dbs {
		dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1", dbPath)
		db, oerr := sql.Open("sqlite3", dsn)
		if oerr != nil {
			continue
		}

		rows, qerr := db.Query(`
			SELECT
				COALESCE(r.ZUNIQUEID, '')       AS uuid,
				COALESCE(r.ZFIRSTNAME, '')      AS first,
				COALESCE(r.ZLASTNAME, '')       AS last,
				COALESCE(r.ZNICKNAME, '')       AS nick,
				COALESCE(r.ZORGANIZATION, '')   AS org,
				COALESCE(p.ZFULLNUMBER, '')     AS phone
			FROM ZABCDPHONENUMBER p
			JOIN ZABCDRECORD r ON r.Z_PK = p.ZOWNER
			WHERE COALESCE(p.ZFULLNUMBER, '') != ''
		`)
		if qerr != nil {
			// ZORGANIZATION is missing on some macOS versions; retry without it.
			rows, qerr = db.Query(`
				SELECT
					COALESCE(r.ZUNIQUEID, '')   AS uuid,
					COALESCE(r.ZFIRSTNAME, '')  AS first,
					COALESCE(r.ZLASTNAME, '')   AS last,
					COALESCE(r.ZNICKNAME, '')   AS nick,
					''                          AS org,
					COALESCE(p.ZFULLNUMBER, '') AS phone
				FROM ZABCDPHONENUMBER p
				JOIN ZABCDRECORD r ON r.Z_PK = p.ZOWNER
				WHERE COALESCE(p.ZFULLNUMBER, '') != ''
			`)
			if qerr != nil {
				db.Close()
				continue
			}
		}

		for rows.Next() {
			var uuid, first, last, nick, org, rawPhone string
			if scanErr := rows.Scan(&uuid, &first, &last, &nick, &org, &rawPhone); scanErr != nil {
				continue
			}
			name := buildContactName(first, last, nick, org)
			if name == "" || uuid == "" {
				continue
			}
			tail := lastTenDigits(rawPhone)
			if tail == "" {
				continue
			}
			e164 := toE164(rawPhone)

			// Get-or-create the canonical MacPerson for this UUID.
			person, exists := personIndex[uuid]
			if !exists {
				person = &MacPerson{UUID: uuid, Name: name}
				personIndex[uuid] = person
			} else if len(name) > len(person.Name) {
				// Prefer the most complete name we see (longer wins).
				person.Name = name
			}

			// Append phone if not already present.
			if !sliceContains(person.Phones, e164) {
				person.Phones = append(person.Phones, e164)
			}

			// First writer wins for the tail map (rare collisions across DBs);
			// matters when the same number appears in multiple sources.
			if _, taken := tailToPerson[tail]; !taken {
				tailToPerson[tail] = person
			}
			totalRows++
		}
		rows.Close()
		db.Close()
	}

	return tailToPerson, personIndex, totalRows, nil
}

// buildContactName picks the best display string for a person record.
// Priority: "First Last" → "First" → "Last" → "Nickname" → "Organization".
func buildContactName(first, last, nick, org string) string {
	first = strings.TrimSpace(first)
	last = strings.TrimSpace(last)
	nick = strings.TrimSpace(nick)
	org = strings.TrimSpace(org)
	switch {
	case first != "" && last != "":
		return first + " " + last
	case first != "":
		return first
	case last != "":
		return last
	case nick != "":
		return nick
	case org != "":
		return org
	}
	return ""
}

// lastTenDigits extracts the trailing-10-digit form for cross-source matching.
func lastTenDigits(phone string) string {
	digits := DigitsOnly(phone)
	if len(digits) < 7 {
		return ""
	}
	if len(digits) > 10 {
		return digits[len(digits)-10:]
	}
	return digits
}

// toE164 returns "+<digits>" form. Defensive against missing leading +.
func toE164(phone string) string {
	digits := DigitsOnly(phone)
	if digits == "" {
		return ""
	}
	return "+" + digits
}

// sliceContains is a tiny helper.
func sliceContains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
