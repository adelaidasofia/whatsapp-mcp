package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importBaileysStore writes a whole baileys_store.json, contacts and
// messages, and runs the real importer over it.
func importBaileysStore(t *testing.T, db *sql.DB, storeJSON string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baileys_store.json")
	if err := os.WriteFile(path, []byte(storeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunBaileysImport(nil, db, path); err != nil {
		t.Fatalf("RunBaileysImport: %v", err)
	}
}

// baileysChats renders a store's "messages" map with one text message per
// chat, which is all the importer needs to write the chat row.
func baileysChats(jids ...string) string {
	chats := make([]string, len(jids))
	for i, jid := range jids {
		chats[i] = fmt.Sprintf(`%q: [{"key": {"remoteJid": %q, "fromMe": false, "id": "M%d"}, "messageTimestamp": 1786000000, "message": {"conversation": "hola"}}]`, jid, jid, i)
	}
	return "{" + strings.Join(chats, ", ") + "}"
}

// listChatNames drives the real GET /api/chats handler and returns the name
// list_chats shows for each chat.
func listChatNames(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	s := &Server{db: db, bridge: &Bridge{}}
	rec := httptest.NewRecorder()
	s.handleListChats(rec, httptest.NewRequest(http.MethodGet, "/api/chats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list chats: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out chatListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list chats: decode response: %v", err)
	}
	names := make(map[string]string, len(out.Chats))
	for _, c := range out.Chats {
		names[c.JID] = c.Name
	}
	return names
}

func contactNames(t *testing.T, db *sql.DB, jid string) (pushName, normalized string) {
	t.Helper()
	err := db.QueryRow(`SELECT COALESCE(push_name, ''), COALESCE(normalized_name, '') FROM contacts WHERE jid = ?`, jid).Scan(&pushName, &normalized)
	if err != nil {
		t.Fatalf("read contact %s: %v", jid, err)
	}
	return pushName, normalized
}

func chatNames(t *testing.T, db *sql.DB, jid string) (name, normalized string) {
	t.Helper()
	err := db.QueryRow(`SELECT COALESCE(name, ''), COALESCE(normalized_name, '') FROM chats WHERE jid = ?`, jid).Scan(&name, &normalized)
	if err != nil {
		t.Fatalf("read chat %s: %v", jid, err)
	}
	return name, normalized
}

// A chat whose store entry carries no name gets the "+<phone>" placeholder
// only when its JID is a phone number, the legacy @c.us form included. The
// digits of a @lid JID are an opaque identifier and a group's are its id, so
// "+<digits>" would present either as a phone number, and the raw JID (the
// old fallback for a JID with no digits) is not a name. Any other chat is
// stored with no name, as NULL the way bridge.go stores it: history_sync.go
// fills a NULL name but keeps an empty string.
func TestBaileysNamelessChatGetsNoMadeUpName(t *testing.T) {
	db := xvDB(t)
	const pn, legacy = "573001234567@s.whatsapp.net", "15555550100@c.us"
	const lid, group, digitless = "123456789012345@lid", "120363000000000001@g.us", "nodigits@lid"
	importBaileysStore(t, db, `{"contacts": {}, "messages": `+baileysChats(pn, legacy, lid, group, digitless)+`}`)

	for jid, want := range map[string]string{pn: "+573001234567", legacy: "+15555550100"} {
		if got, _ := chatNames(t, db, jid); got != want {
			t.Errorf("%s: chat name = %q, want the placeholder %q", jid, got, want)
		}
	}
	for _, jid := range []string{lid, group, digitless} {
		var name, normalized sql.NullString
		err := db.QueryRow(`SELECT name, normalized_name FROM chats WHERE jid = ?`, jid).Scan(&name, &normalized)
		if err != nil {
			t.Fatalf("read chat %s: %v", jid, err)
		}
		if name.Valid || normalized.Valid {
			t.Errorf("%s: name = %+v, normalized_name = %+v; want both NULL", jid, name, normalized)
		}
	}
}

// A store with no name for a chat must leave the stored name alone, and
// normalized_name has to make the same keep-or-replace choice as name. Every
// other chat writer moves the two together. The importer's upsert kept the
// name but always overwrote normalized_name, so a chat arriving with no name
// would keep its name and lose the normalized form.
func TestBaileysReimportKeepsAChatNameAndItsNormalizedForm(t *testing.T) {
	db := xvDB(t)
	const lid, group = "123456789012345@lid", "120363000000000001@g.us"
	xvChat(t, db, group, "group", "Familia Pérez", 1785000000)
	xvChat(t, db, lid, "direct", "Mi Amor", 1785000000)
	store := `{"contacts": {}, "messages": ` + baileysChats(lid, group) + `}`

	for run := 1; run <= 2; run++ {
		importBaileysStore(t, db, store)
		for jid, want := range map[string]string{group: "Familia Pérez", lid: "Mi Amor"} {
			name, normalized := chatNames(t, db, jid)
			if name != want || normalized != Normalize(want) {
				t.Errorf("import %d, %s: name = %q, normalized_name = %q; want %q, %q",
					run, jid, name, normalized, want, Normalize(want))
			}
		}
	}
}

// A store with no name for a contact must not replace a real name, neither
// with the "+<phone>" placeholder (the shape export_vault.go and
// crm_enrich.go read as "no name yet") nor with nothing. normalized_name has
// to follow the push_name that is kept: search_contacts matches names through
// it, never through push_name.
func TestBaileysReimportKeepsAContactsRealName(t *testing.T) {
	db := xvDB(t)
	const pn, lid = "573001234567@s.whatsapp.net", "123456789012345@lid"
	xvContact(t, db, pn, "Juan Pérez", "573001234567")
	xvContact(t, db, lid, "Bea", "")
	nameless := `{
		"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net"},
		"123456789012345@lid": {"id": "123456789012345@lid"}
	}`

	for run := 1; run <= 2; run++ {
		runBaileysImport(t, db, nameless)
		for jid, want := range map[string]string{pn: "Juan Pérez", lid: "Bea"} {
			name, normalized := contactNames(t, db, jid)
			if name != want || normalized != Normalize(want) {
				t.Errorf("import %d, %s: push_name = %q, normalized_name = %q; want %q, %q",
					run, jid, name, normalized, want, Normalize(want))
			}
		}
		for q, jid := range map[string]string{"juan perez": pn, "bea": lid} {
			found := false
			for _, c := range searchContacts(t, db, q) {
				found = found || c.JID == jid
			}
			if !found {
				t.Errorf("import %d: search_contacts %q does not return %s", run, q, jid)
			}
		}
	}
}

// The placeholder must not replace a real chat name either. A direct chat the
// address book named keeps that name when a store with no name for the
// contact is imported over it, while the contact, which has no push_name of
// its own, still gets the placeholder.
func TestBaileysReimportKeepsARealDirectChatName(t *testing.T) {
	db := xvDB(t)
	const pn = "573001234567@s.whatsapp.net"
	xvChat(t, db, pn, "direct", "", 1785000000)
	if _, err := writeContactName(t.Context(), db, pn, "Mi Amor", 1785000000); err != nil {
		t.Fatalf("writeContactName: %v", err)
	}
	store := `{"contacts": {"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net"}}, "messages": ` + baileysChats(pn) + `}`

	for run := 1; run <= 2; run++ {
		importBaileysStore(t, db, store)
		if got := listChatNames(t, db)[pn]; got != "Mi Amor" {
			t.Errorf("import %d: list_chats shows %q, want the address-book name %q", run, got, "Mi Amor")
		}
		if name, normalized := chatNames(t, db, pn); normalized != Normalize(name) {
			t.Errorf("import %d: chat normalized_name = %q for name %q", run, normalized, name)
		}
		if name, _ := contactNames(t, db, pn); name != "+573001234567" {
			t.Errorf("import %d: contact push_name = %q, want the placeholder %q", run, name, "+573001234567")
		}
	}
}

// Only a real stored name is protected. The placeholder still fills a
// contact or chat with no name, and a name from the store still replaces a
// placeholder an earlier import wrote.
func TestBaileysPlaceholderStillFillsAMissingName(t *testing.T) {
	db := xvDB(t)
	const named, blank = "573001234567@s.whatsapp.net", "573009876543@s.whatsapp.net"
	xvContact(t, db, named, "+573001234567", "573001234567")
	xvContact(t, db, blank, "", "573009876543")
	xvChat(t, db, named, "direct", "+573001234567", 1785000000)
	xvChat(t, db, blank, "direct", "", 1785000000)
	importBaileysStore(t, db, `{"contacts": {
		"573001234567@s.whatsapp.net": {"id": "573001234567@s.whatsapp.net", "notify": "Ana"},
		"573009876543@s.whatsapp.net": {"id": "573009876543@s.whatsapp.net"}
	}, "messages": `+baileysChats(named, blank)+`}`)

	for jid, want := range map[string]string{named: "Ana", blank: "+573009876543"} {
		if name, normalized := contactNames(t, db, jid); name != want || normalized != Normalize(want) {
			t.Errorf("contact %s: push_name = %q, normalized_name = %q; want %q, %q", jid, name, normalized, want, Normalize(want))
		}
		if name, normalized := chatNames(t, db, jid); name != want || normalized != Normalize(want) {
			t.Errorf("chat %s: name = %q, normalized_name = %q; want %q, %q", jid, name, normalized, want, Normalize(want))
		}
	}
}
