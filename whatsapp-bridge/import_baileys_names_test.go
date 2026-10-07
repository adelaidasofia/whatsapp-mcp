package main

import (
	"database/sql"
	"fmt"
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

func chatNames(t *testing.T, db *sql.DB, jid string) (name, normalized string) {
	t.Helper()
	err := db.QueryRow(`SELECT COALESCE(name, ''), COALESCE(normalized_name, '') FROM chats WHERE jid = ?`, jid).Scan(&name, &normalized)
	if err != nil {
		t.Fatalf("read chat %s: %v", jid, err)
	}
	return name, normalized
}

// A chat whose store entry carries no name gets the "+<phone>" placeholder
// only when its JID is a phone number. The digits of a @lid JID are an opaque
// identifier and a group's are its id, so "+<digits>" would present either as
// a phone number, and the raw JID is not a name.
func TestBaileysNamelessChatGetsNoMadeUpName(t *testing.T) {
	db := xvDB(t)
	const pn, lid, group = "573001234567@s.whatsapp.net", "123456789012345@lid", "120363000000000001@g.us"
	importBaileysStore(t, db, `{"contacts": {}, "messages": `+baileysChats(pn, lid, group)+`}`)

	want := map[string]string{pn: "+573001234567", lid: "", group: ""}
	for jid, w := range want {
		if got, _ := chatNames(t, db, jid); got != w {
			t.Errorf("%s: chat name = %q, want %q", jid, got, w)
		}
	}
}

// A store with no name for a chat must leave the stored name alone, and
// normalized_name has to make the same keep-or-replace choice as name. Every
// other chat writer moves the two together; the importer kept the name and
// blanked its normalized form.
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
