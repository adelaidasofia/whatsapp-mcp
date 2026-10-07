package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

// Characters that turn up in numbers copied from a phone or a chat. Built
// from code points so the test source stays plain ASCII.
var (
	ltrEmbedding     = string(rune(0x202a)) // invisible direction mark
	popDirectional   = string(rune(0x202c)) // closes the mark above
	noBreakSpace     = string(rune(0x00a0))
	nonBreakingDash  = string(rune(0x2011))
	enDash           = string(rune(0x2013))
	testNumberDigits = "573001234567"
)

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
		"573001234567",     // the number as stored
		"3001234567",       // without the country code
		"+573001234567",    // E.164
		"+57 300 123 4567", // as a phone displays it
		"(300) 123-4567",   // local formatting
		"57 300/123 4567",  // slash-separated
		ltrEmbedding + "+57 300 1234567" + popDirectional,
		"+57" + noBreakSpace + "300" + noBreakSpace + "123" + noBreakSpace + "4567",
		"+57 300" + nonBreakingDash + "123" + nonBreakingDash + "4567",
		"300" + enDash + "1234567",
	} {
		got := searchContacts(t, db, q)
		if len(got) != 1 || got[0].JID != jid {
			t.Errorf("search %q: got %+v, want exactly %s", q, got, jid)
			continue
		}
		if got[0].Phone != testNumberDigits {
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

// On a @lid row the phone column is the only place the number lives; the JID
// match cannot reach it. A formatted query must match that column on its
// digits too. This is the row shape bridge.go writes when a message arrives
// from the @lid form with the phone-number form as its alias.
func TestFormattedNumberMatchesThePhoneColumn(t *testing.T) {
	db := newContactTestDB(t)
	const lid = "123456789012345@lid"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, phone, is_business, created_at, updated_at)
		VALUES (?, ?, 0, 0, 0)
	`, lid, testNumberDigits); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := searchContacts(t, db, "+57 300 123 4567"); len(got) != 1 || got[0].JID != lid {
		t.Fatalf("got %+v, want %s found through its phone column", got, lid)
	}
}

// The other half of the phone-column rule: the user part of a @lid JID is an
// opaque identifier, not a number anyone dials. An address-book entry keyed
// by LID must not get it as its phone, and searching those digits must not
// return the row as a number match.
func TestAddressBookLIDIsNotStoredOrMatchedAsAPhone(t *testing.T) {
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

// A @lid row can already carry the real phone: bridge.go stores it from the
// message's phone-number alias, and the alias backfill repairs it. An
// address-book write for that row has no phone of its own to offer and must
// not erase the one already there.
func TestAddressBookWriteKeepsAPhoneLearnedElsewhere(t *testing.T) {
	db := newContactTestDB(t)
	const lid = "123456789012345@lid"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, phone, is_business, created_at, updated_at)
		VALUES (?, ?, 0, 0, 0)
	`, lid, testNumberDigits); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := writeContactName(t.Context(), db, lid, "Plomero Casa", 100); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}
	var phone sql.NullString
	if err := db.QueryRow(`SELECT phone FROM contacts WHERE jid = ?`, lid).Scan(&phone); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !phone.Valid || phone.String != testNumberDigits {
		t.Fatalf("phone = %+v after the address-book write, want %s kept", phone, testNumberDigits)
	}
}

// A short number-shaped query can match many rows by phone. The rows that
// match by name must still come first, or the default limit of 10 drops
// them: "7-" finds "7-Eleven" by name, and the number clauses also match
// every phone with a 7 in it. Both name columns count: the self-chosen name
// and the one saved in the address book.
func TestNameMatchesRankAheadOfNumberMatches(t *testing.T) {
	db := newContactTestDB(t)
	// Both name matches are the oldest rows, so ordering by recency alone
	// would put them last. Neither JID nor phone contains a 7.
	const pushNamed = "15555550100@s.whatsapp.net"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, phone, push_name, normalized_name, is_business, created_at, updated_at)
		VALUES (?, '15555550100', '7-Eleven', ?, 0, 0, 1)
	`, pushNamed, Normalize("7-Eleven")); err != nil {
		t.Fatalf("seed push-name match: %v", err)
	}
	const addressBookNamed = "15555550101@s.whatsapp.net"
	if _, err := writeContactName(t.Context(), db, addressBookNamed, "7-Eleven Centro", 1); err != nil {
		t.Fatalf("seed address-book match: %v", err)
	}
	for i := 0; i < 12; i++ {
		digits := fmt.Sprintf("1555557%04d", i)
		if _, err := db.Exec(`
			INSERT INTO contacts (jid, phone, is_business, created_at, updated_at)
			VALUES (?, ?, 0, 0, ?)
		`, digits+"@s.whatsapp.net", digits, 100+i); err != nil {
			t.Fatalf("seed number match %d: %v", i, err)
		}
	}

	got := searchContacts(t, db, "7-")
	if len(got) < 2 {
		t.Fatalf("got %d results, want both name matches first: %+v", len(got), got)
	}
	first := map[string]bool{got[0].JID: true, got[1].JID: true}
	if !first[pushNamed] || !first[addressBookNamed] {
		t.Fatalf("first two results = %s, %s; want the name matches %s and %s ahead of the number matches",
			got[0].JID, got[1].JID, pushNamed, addressBookNamed)
	}
}

// Among rows that match only by number, the newest comes first whatever
// their name columns hold. A rank expression that evaluates to NULL when a
// name column is empty and to 0 when both are set (SQL's three-valued LIKE)
// put an old, fully named contact ahead of a newer one.
func TestNumberOnlyMatchesKeepRecencyOrder(t *testing.T) {
	db := newContactTestDB(t)
	const older = "15555550201@s.whatsapp.net"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, phone, push_name, normalized_name, full_name, normalized_full_name,
		                      is_business, created_at, updated_at)
		VALUES (?, '15555550201', 'Olga', 'olga', 'Olga P', 'olga p', 0, 0, 1)
	`, older); err != nil {
		t.Fatalf("seed older: %v", err)
	}
	const newer = "15555550202@s.whatsapp.net"
	if _, err := db.Exec(`
		INSERT INTO contacts (jid, phone, is_business, created_at, updated_at)
		VALUES (?, '15555550202', 0, 0, 2)
	`, newer); err != nil {
		t.Fatalf("seed newer: %v", err)
	}

	got := searchContacts(t, db, "555555020")
	if len(got) != 2 || got[0].JID != newer || got[1].JID != older {
		t.Fatalf("got %+v, want [%s, %s] (newest first)", got, newer, older)
	}
}

// phoneFromJID decides what every writer that uses it stores as a phone.
// Each guard is pinned here, including the ones only a malformed import can
// reach (an empty user part, a user part that is not a number).
func TestPhoneFromJID(t *testing.T) {
	none := sql.NullString{}
	phone := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	for _, tc := range []struct {
		jid  string
		want sql.NullString
	}{
		{"573001234567@s.whatsapp.net", phone("573001234567")},
		{"573001234567:12@s.whatsapp.net", phone("573001234567")}, // device suffix split off first
		{"15555550100@c.us", phone("15555550100")},                // legacy phone-number form
		{"123456789012345@lid", none},                             // opaque identifier, not a number
		{"120363000000000000@g.us", none},                         // a group
		{"@s.whatsapp.net", none},                                 // empty user part
		{"57300abc@s.whatsapp.net", none},                         // user part is not a bare number
		{"not-a-jid", none},                                       // no server at all
	} {
		if got := phoneFromJID(tc.jid); got != tc.want {
			t.Errorf("phoneFromJID(%q) = %+v, want %+v", tc.jid, got, tc.want)
		}
	}
}

// CRM enrichment matches contacts by phone, so it never saw an address-book
// contact while that contact's number lived only in the JID. With phone
// filled it reaches them under its existing rules, as it already did for a
// @lid address-book row whose phone the alias backfill had filled: a blank
// push_name is filled from the CRM, and a name the contact chose for
// themselves is left alone.
func TestCRMEnrichmentReachesAddressBookContactsByPhone(t *testing.T) {
	db := xvDB(t)
	const saved = "573001234567@s.whatsapp.net"
	if _, err := writeContactName(t.Context(), db, saved, "Mi Amor", 100); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}
	const selfNamed = "15555550100@s.whatsapp.net"
	xvContact(t, db, selfNamed, "Dra Ivette", "15555550100")

	crm := t.TempDir()
	for file, body := range map[string]string{
		"Ivette De La Vega.md": "---\nphone: \"+57 300 123 4567\"\n---\n",
		"Ana Gomez.md":         "---\nphone: 15555550100\n---\n",
	} {
		if err := os.WriteFile(filepath.Join(crm, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := EnrichContactsFromVault(db, crm)
	if err != nil {
		t.Fatalf("EnrichContactsFromVault: %v", err)
	}

	pushName := func(jid string) string {
		t.Helper()
		var name string
		if err := db.QueryRow(`SELECT COALESCE(push_name, '') FROM contacts WHERE jid = ?`, jid).Scan(&name); err != nil {
			t.Fatalf("read %s: %v", jid, err)
		}
		return name
	}
	if got := pushName(saved); got != "Ivette De La Vega" {
		t.Errorf("address-book contact push_name = %q, want the CRM name: enrichment matches on phone", got)
	}
	if got := pushName(selfNamed); got != "Dra Ivette" {
		t.Errorf("self-named contact push_name = %q, want it left alone", got)
	}
	if updated != 1 {
		t.Errorf("enrichment updated %d rows, want 1", updated)
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
		{"15555550133@s.whatsapp.net", "other-value"}, // set to something else: not the migration's to rewrite
		{"123456789012345@lid", nil},                  // LID: no phone to derive
		{"15555550122:3@s.whatsapp.net", nil},         // device suffix: not a bare number
		{"120363000000000000@g.us", nil},              // not a person
		{"@s.whatsapp.net", nil},                      // empty user part, as a malformed import could write
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
		"15555550133@s.whatsapp.net":   {String: "other-value", Valid: true},
		"123456789012345@lid":          {},
		"15555550122:3@s.whatsapp.net": {},
		"120363000000000000@g.us":      {},
		"@s.whatsapp.net":              {},
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

// runBaileysImport writes a baileys_store.json with the given contacts map
// and runs the real importer over it.
func runBaileysImport(t *testing.T, db *sql.DB, contactsJSON string) {
	t.Helper()
	store := `{"contacts": ` + contactsJSON + `, "messages": {}}`
	path := filepath.Join(t.TempDir(), "baileys_store.json")
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunBaileysImport(nil, db, path); err != nil {
		t.Fatalf("RunBaileysImport: %v", err)
	}
}

// The Baileys importer derived phone with extractPhone, which keeps the
// digits of ANY JID, so a @lid contact was imported with its LID stored as
// its phone number: the exact row aliases.go's SuspiciousLIDPhones counts.
// The legacy @c.us form is a phone-number JID and keeps its phone.
func TestBaileysImportStoresPhoneOnlyForPhoneNumberJIDs(t *testing.T) {
	db := newContactTestDB(t)
	runBaileysImport(t, db, `{
		"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net", "notify": "Ana"},
		"123456789012345@lid": {"id": "123456789012345@lid", "notify": "Bea"},
		"15555550100@c.us": {"id": "15555550100@c.us", "notify": "Cata"}
	}`)

	want := map[string]sql.NullString{
		"573001234567@s.whatsapp.net": {String: "573001234567", Valid: true},
		"123456789012345@lid":         {},
		"15555550100@c.us":            {String: "15555550100", Valid: true},
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

// A Baileys contact with no name of any kind gets the "+<phone>" placeholder
// only when it has a phone. Anything else gets no label: not "+<LID digits>",
// which presents an identifier as a phone number, and not the raw JID, which
// the vault export shows ahead of the name the user saved.
func TestBaileysNamelessContactGetsNoMadeUpName(t *testing.T) {
	db := xvDB(t)
	const pn = "573001234567@s.whatsapp.net"
	const lid = "123456789012345@lid"
	runBaileysImport(t, db, `{
		"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net"},
		"123456789012345@lid": {"id": "123456789012345@lid"}
	}`)

	label := func(jid string) string {
		t.Helper()
		var name string
		if err := db.QueryRow(`SELECT COALESCE(push_name, '') FROM contacts WHERE jid = ?`, jid).Scan(&name); err != nil {
			t.Fatalf("read %s: %v", jid, err)
		}
		return name
	}
	if got := label(pn); got != "+573001234567" {
		t.Errorf("phone-number contact label = %q, want the +<phone> placeholder", got)
	}
	if got := label(lid); got != "" {
		t.Errorf("LID contact label = %q, want none", got)
	}

	// End to end: the user saved the LID contact as "Mi Amor", so the export
	// must say "Mi Amor".
	const ts = int64(1786000000)
	xvChat(t, db, lid, "direct", "", ts)
	xvMsg(t, db, "M1", lid, lid, "", "text", "hola", "", ts, false)
	if _, err := writeContactName(t.Context(), db, lid, "Mi Amor", ts); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}
	units, _, err := buildExportUnits(db, false, 0, nil)
	if err != nil {
		t.Fatalf("buildExportUnits: %v", err)
	}
	for _, u := range units {
		if u.primary == lid {
			if u.display != "Mi Amor" {
				t.Fatalf("export display = %q, want the address-book name %q", u.display, "Mi Amor")
			}
			return
		}
	}
	t.Fatalf("no export unit for %s among %d units", lid, len(units))
}
