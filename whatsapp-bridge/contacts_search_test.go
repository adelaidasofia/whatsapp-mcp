package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// searchContacts drives the real GET /api/contacts/search handler. The test
// that guarded this endpoint used to copy part of its WHERE clause instead,
// so the number half of the search was never exercised by anything.
func searchContacts(t *testing.T, db *sql.DB, q string) []contactRow {
	t.Helper()
	s := &Server{db: db, bridge: &Bridge{}}
	req := httptest.NewRequest(http.MethodGet, "/api/contacts/search?q="+url.QueryEscape(q), nil)
	rec := httptest.NewRecorder()
	s.handleSearchContacts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %q: HTTP %d: %s", q, rec.Code, rec.Body.String())
	}
	var out contactListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("search %q: decode response: %v", q, err)
	}
	return out.Contacts
}

// The reported failure: a contact saved in the phone's address book is found
// by name and not by number. The row comes from the address-book writer
// alone, as it does on a real install for anyone the bridge has not also
// learned about from a message.
func TestSearchFindsAnAddressBookContactByNumber(t *testing.T) {
	db := newContactTestDB(t)
	const jid = "573001234567@s.whatsapp.net"
	if _, err := writeContactName(t.Context(), db, jid, "Plomero Casa", 100); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}

	for _, q := range []string{
		"573001234567",      // the number as stored
		"3001234567",        // without the country code
		"+573001234567",     // E.164
		"+57 300 123 4567",  // as a phone displays it
		"(300) 123-4567",    // local formatting
		"‪+57 300 1234567‬", // wrapped in invisible direction marks
		"+57 300 123 4567",  // no-break spaces
	} {
		got := searchContacts(t, db, q)
		if len(got) != 1 || got[0].JID != jid {
			t.Errorf("search %q: got %+v, want exactly %s", q, got, jid)
			continue
		}
		if got[0].Phone != "573001234567" {
			t.Errorf("search %q: phone = %q, want the number carried by the JID", q, got[0].Phone)
		}
	}

	// Searching by name keeps working; this adds a way in, it does not
	// replace one.
	if got := searchContacts(t, db, "plomero"); len(got) != 1 {
		t.Errorf("name search returned %d contacts, want 1", len(got))
	}
}

// The search must not depend on every writer remembering the phone column.
// A row that carries its number only in the JID, which is how the
// address-book writer stored every row before it wrote phone, is still found
// by number.
func TestNumberSearchReadsTheJIDWhenPhoneIsMissing(t *testing.T) {
	db := newContactTestDB(t)
	const jid = "15555550100@s.whatsapp.net"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, is_business, created_at, updated_at)
		VALUES (?, 0, 0, 0)
	`, jid); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := searchContacts(t, db, "5555550100"); len(got) != 1 || got[0].JID != jid {
		t.Fatalf("got %+v, want %s found through its JID", got, jid)
	}
}

// The other half of the phone-column rule: the user part of a @lid JID is an
// opaque identifier, not a number anyone dials. An address-book entry keyed
// by LID must not get it as its phone, and searching those digits must not
// return the row as a number match.
func TestAddressBookLIDIsNeverStoredOrMatchedAsAPhone(t *testing.T) {
	db := newContactTestDB(t)
	const lid = "123456789012345@lid"
	if _, err := writeContactName(t.Context(), db, lid, "Plomero Casa", 100); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}
	var phone sql.NullString
	if err := db.QueryRow(`SELECT phone FROM contacts WHERE jid = ?`, lid).Scan(&phone); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if phone.Valid {
		t.Fatalf("LID row got phone %q; a LID is not a phone number", phone.String)
	}
	if got := searchContacts(t, db, "123456789012345"); len(got) != 0 {
		t.Fatalf("searching the LID digits returned %+v, want no number match", got)
	}
}

// Migration 008 repairs the rows the address-book writer already wrote with
// phone NULL. It fills only phone-number JIDs whose user part is a bare
// number, leaves every other row as it was, and does not touch updated_at,
// which search results are ordered by.
func TestMigration008FillsPhoneFromPhoneNumberJIDs(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	// The pre-008 state, reached the way a real install reached it.
	applyMigrationsUpTo(t, db, 7)

	seed := []struct {
		jid   string
		phone any
	}{
		{"573001234567@s.whatsapp.net", nil},          // address-book row: the reported case
		{"15555550100@s.whatsapp.net", ""},            // empty string reads as missing
		{"15555550111@s.whatsapp.net", "15555550111"}, // already right
		{"123456789012345@lid", nil},                  // LID: no phone to derive
		{"15555550122:3@s.whatsapp.net", nil},         // device suffix: not a bare number
		{"120363000000000000@g.us", nil},              // not a person
	}
	for _, s := range seed {
		if _, err := db.Exec(`
			INSERT INTO contacts (jid, phone, is_business, created_at, updated_at)
			VALUES (?, ?, 0, 0, 42)
		`, s.jid, s.phone); err != nil {
			t.Fatalf("seed %s: %v", s.jid, err)
		}
	}

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations to head: %v", err)
	}

	want := map[string]sql.NullString{
		"573001234567@s.whatsapp.net":  {String: "573001234567", Valid: true},
		"15555550100@s.whatsapp.net":   {String: "15555550100", Valid: true},
		"15555550111@s.whatsapp.net":   {String: "15555550111", Valid: true},
		"123456789012345@lid":          {},
		"15555550122:3@s.whatsapp.net": {},
		"120363000000000000@g.us":      {},
	}
	for jid, w := range want {
		var got sql.NullString
		var updated int64
		if err := db.QueryRow(`SELECT phone, updated_at FROM contacts WHERE jid = ?`, jid).Scan(&got, &updated); err != nil {
			t.Fatalf("read %s: %v", jid, err)
		}
		if got != w {
			t.Errorf("%s: phone = %+v, want %+v", jid, got, w)
		}
		if updated != 42 {
			t.Errorf("%s: updated_at changed to %d; a backfill is not activity", jid, updated)
		}
	}

	var recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 8`).Scan(&recorded); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("schema_version 8 recorded %d times, want 1", recorded)
	}
}

// The Baileys importer derived phone with extractPhone, which keeps the
// digits of ANY JID, so a @lid contact was imported with its LID stored as
// its phone number: the exact row aliases.go's SuspiciousLIDPhones counts.
func TestBaileysImportStoresPhoneOnlyForPhoneNumberJIDs(t *testing.T) {
	db := newContactTestDB(t)
	store := `{
		"contacts": {
			"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net", "notify": "Ana"},
			"123456789012345@lid": {"id": "123456789012345@lid", "notify": "Bea"}
		},
		"messages": {}
	}`
	path := filepath.Join(t.TempDir(), "baileys_store.json")
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunBaileysImport(nil, db, path); err != nil {
		t.Fatalf("RunBaileysImport: %v", err)
	}

	want := map[string]sql.NullString{
		"573001234567@s.whatsapp.net": {String: "573001234567", Valid: true},
		"123456789012345@lid":         {},
	}
	for jid, w := range want {
		var got sql.NullString
		if err := db.QueryRow(`SELECT phone FROM contacts WHERE jid = ?`, jid).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", jid, err)
		}
		if got != w {
			t.Errorf("%s: phone = %+v, want %+v", jid, got, w)
		}
	}
}
