package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Windows-safety cases for exported chat filenames: reserved device stems
// and trailing dots/spaces produce files Windows refuses to create (or, for
// NUL, silently swallows). Same class shipped in whatsapp-vault-sync.
func TestSanitizeFilenameWindowsSafety(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Maria/Jose", "Maria-Jose"}, // path separators (pre-existing behavior)
		{"a\\b?c%d*e:f|g\"h<i>j[k]", "a-b-c-d-e-f-g-h-i-j-k-"},
		{"CON", "CON_"},
		{"con", "con_"},
		{"NUL", "NUL_"},
		{"COM1", "COM1_"},
		{"lpt9", "lpt9_"},
		{"COM0", "COM0"},           // 0 is not reserved
		{"CONTRERAS", "CONTRERAS"}, // longer than the device stem
		{"name...", "name"},
		{"name   ", "name"},
		{"", "Unknown"},
		{". .", "Unknown"},
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatPairingCode(t *testing.T) {
	if got := FormatPairingCode("ABCD1234"); got != "ABCD-1234" {
		t.Errorf("FormatPairingCode(ABCD1234) = %q", got)
	}
	if got := FormatPairingCode("ABC"); got != "ABC" {
		t.Errorf("FormatPairingCode(ABC) = %q", got)
	}
	// Already-hyphenated input from whatsmeow stays stable.
	if got := FormatPairingCode("ABCD-1234"); got != "ABCD-1234" {
		t.Errorf("FormatPairingCode(ABCD-1234) = %q", got)
	}
}

// --- vault export must never render an undecodable message as nothing -------

// TestExportRendersUndecodableAsPlaceholder is the end-of-pipeline guard for
// MYC-3284. The decoder fix is worth nothing if the export still drops the row:
// before this, exportOneChat ended every message with `if text == "" { continue }`,
// so an undecodable message produced no line at all. The markdown looked
// healthy, the dates were right, and the message simply was not there.
func TestExportRendersUndecodableAsPlaceholder(t *testing.T) {
	db := freshMigratedDB(t)
	seedChatWithMessages(t, db, "u@s.whatsapp.net", "Test Contact", []seedMsg{
		{id: "m1", ts: 1784846600, typ: "text", text: "before", rawType: "conversation"},
		{id: "m2", ts: 1784846683, typ: "unsupported", text: "", rawType: "templateMessage"},
		{id: "m3", ts: 1784846700, typ: "text", text: "after", rawType: "conversation"},
	})

	outDir := t.TempDir()
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	body := readExportedChat(t, outDir)

	// The surrounding messages must still be there, so a failure below is
	// clearly about the placeholder and not a broken export.
	for _, want := range []string{"before", "after"} {
		if !strings.Contains(body, want) {
			t.Fatalf("export lost ordinary message %q; output:\n%s", want, body)
		}
	}

	if !strings.Contains(body, "Unsupported message") {
		t.Errorf("undecodable message produced no placeholder line — it is invisible in the vault.\nGot:\n%s", body)
	}
	// The raw type is what makes the placeholder actionable rather than just
	// an admission that something is missing.
	if !strings.Contains(body, "templateMessage") {
		t.Errorf("placeholder does not name the raw type, so nothing says what to decode next.\nGot:\n%s", body)
	}
}

// TestExportRendersPollAndEvent covers the two new first-class types.
func TestExportRendersPollAndEvent(t *testing.T) {
	db := freshMigratedDB(t)
	seedChatWithMessages(t, db, "p@s.whatsapp.net", "Poll Contact", []seedMsg{
		{id: "p1", ts: 1784846600, typ: "poll", text: "Ship it?\n- Yes\n- No", rawType: "pollCreationMessageV3"},
		{id: "p2", ts: 1784846700, typ: "event", text: "Launch review", rawType: "eventMessage"},
	})

	outDir := t.TempDir()
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	body := readExportedChat(t, outDir)

	if !strings.Contains(body, "[Poll]") || !strings.Contains(body, "Ship it?") {
		t.Errorf("poll not rendered.\nGot:\n%s", body)
	}
	if !strings.Contains(body, "[Event]") || !strings.Contains(body, "Launch review") {
		t.Errorf("event not rendered.\nGot:\n%s", body)
	}
}

// TestExportSkipsOnlyGenuineProtocolRows: an allowlisted `system` row carries no
// user-visible text and should stay out of the markdown, so the export does not
// fill up with key-exchange noise. This is the one case where silence is correct.
func TestExportSkipsOnlyGenuineProtocolRows(t *testing.T) {
	db := freshMigratedDB(t)
	seedChatWithMessages(t, db, "s@s.whatsapp.net", "Sys Contact", []seedMsg{
		{id: "s1", ts: 1784846600, typ: "text", text: "real message", rawType: "conversation"},
		{id: "s2", ts: 1784846650, typ: "system", text: "", rawType: "senderKeyDistributionMessage"},
	})

	outDir := t.TempDir()
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	body := readExportedChat(t, outDir)

	if !strings.Contains(body, "real message") {
		t.Fatalf("export lost the real message.\nGot:\n%s", body)
	}
	if strings.Contains(body, "senderKeyDistributionMessage") {
		t.Errorf("protocol row leaked into the vault export as a placeholder.\nGot:\n%s", body)
	}
}

type seedMsg struct {
	id      string
	ts      int64
	typ     string
	text    string
	rawType string
}

func seedChatWithMessages(t *testing.T, db *sql.DB, jid, pushName string, msgs []seedMsg) {
	t.Helper()
	var last int64
	for _, m := range msgs {
		if m.ts > last {
			last = m.ts
		}
	}
	if _, err := db.Exec(
		`INSERT INTO chats (jid, chat_type, name, created_at, updated_at, last_message_time) VALUES (?,?,?,?,?,?)`,
		jid, "direct", pushName, 0, 0, last,
	); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO contacts (jid, push_name, is_business, created_at, updated_at) VALUES (?,?,0,0,0)`,
		jid, pushName,
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	for _, m := range msgs {
		if _, err := db.Exec(
			`INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, is_from_me, raw_type)
			 VALUES (?,?,?,?,?,?,?,0,?)`,
			m.id, jid, jid, pushName, m.ts, m.typ, m.text, m.rawType,
		); err != nil {
			t.Fatalf("seed message %s: %v", m.id, err)
		}
	}
}

func readExportedChat(t *testing.T, outDir string) string {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read export dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			b, err := os.ReadFile(filepath.Join(outDir, e.Name()))
			if err != nil {
				t.Fatalf("read export file: %v", err)
			}
			return string(b)
		}
	}
	t.Fatal("no .md file written by ExportVault")
	return ""
}
