package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// MYC-3555 — the vault export dropped entire DIRECT chats and filed group
// chats under person names, while exiting 0.
//
// Mechanism (all three interact):
//  1. Filenames are keyed on DISPLAY NAME only. A group whose chats.name holds
//     a person's push name (bridge.go used to store the first sender's name as
//     the group name) collides with that person's direct chat on ONE path.
//  2. The unchanged-skip trusted last_message_ts of WHATEVER file sat at the
//     path, without checking whose jid the file carries. An active group
//     occupying the person's filename therefore suppressed the person's direct
//     chat forever: writeResultUnchanged, no file, rc=0.
//  3. The exporter never consulted jid_aliases, so the LID and phone forms of
//     the same human exported as two colliding units, last-writer-wins, each
//     holding only its own scheme's half of the history.
//
// Every test in this file drives the PUBLIC ExportVault entry point only, so
// the file compiles unchanged against the pre-fix exporter — that is what makes
// the red run provable. Fixture identities are synthetic (555 numbers, made-up
// LIDs); real names and numbers must never appear here (personal-pii-scrub.yml).

const (
	xvPersonName = "Alex Rivera"
	xvLIDJID     = "84930125550023@lid"
	xvPhoneJID   = "15555550100@s.whatsapp.net"
	xvGroupJID   = "120363000000000001@g.us"

	xvGroupLastTS = int64(1785000000)
	xvLIDLastTS   = int64(1784999000)
	xvPhoneLastTS = int64(1784998000)

	xvTranscript = "the missing voice note text"
)

func xvDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // mirror production: SQLite is single-writer.
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db
}

func xvChat(t *testing.T, db *sql.DB, jid, chatType, name string, lastTS int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO chats (jid, chat_type, name, normalized_name, last_message_time, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, jid, chatType, name, Normalize(name), lastTS, lastTS, lastTS)
	if err != nil {
		t.Fatalf("insert chat %s: %v", jid, err)
	}
}

func xvContact(t *testing.T, db *sql.DB, jid, pushName, phone string) {
	t.Helper()
	var phoneVal any
	if phone != "" {
		phoneVal = phone
	}
	_, err := db.Exec(`
		INSERT INTO contacts (jid, phone, push_name, normalized_name, is_business, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, 1, 1)
	`, jid, phoneVal, pushName, Normalize(pushName))
	if err != nil {
		t.Fatalf("insert contact %s: %v", jid, err)
	}
}

func xvMsg(t *testing.T, db *sql.DB, id, chatJID, senderJID, senderDisplay, msgType, text, transcript string, ts int64, fromMe bool) {
	t.Helper()
	var transcriptVal any
	if transcript != "" {
		transcriptVal = transcript
	}
	_, err := db.Exec(`
		INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, is_from_me, voice_note_transcript)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, chatJID, senderJID, senderDisplay, ts, msgType, text, boolToInt(fromMe), transcriptVal)
	if err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

func xvAlias(t *testing.T, db *sql.DB, a, b string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO jid_aliases (jid_a, jid_b, discovered_at, source)
		VALUES (?, ?, 1, 'test'), (?, ?, 1, 'test')
		ON CONFLICT(jid_a, jid_b) DO NOTHING
	`, a, b, b, a)
	if err != nil {
		t.Fatalf("insert alias %s<->%s: %v", a, b, err)
	}
}

// xvReadFile splits a chat file into frontmatter key→raw-value and body.
func xvReadFile(t *testing.T, path string) (map[string]string, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		t.Fatalf("%s has no frontmatter:\n%s", path, content)
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		t.Fatalf("%s frontmatter unterminated", path)
	}
	fm := map[string]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		fm[strings.TrimSpace(line[:idx])] = strings.Trim(strings.TrimSpace(line[idx+1:]), `"`)
	}
	return fm, rest[end:]
}

// xvFileForJID scans outDir for the chat file whose frontmatter jid equals the
// given JID. Returns "" when no file claims it.
func xvFileForJID(t *testing.T, outDir, jid string) string {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm, _ := xvReadFile(t, filepath.Join(outDir, e.Name()))
		if fm["jid"] == jid {
			return e.Name()
		}
	}
	return ""
}

// xvSeedFounderShape reproduces the measured 2026-07-31 vault state with
// synthetic identities: one human reachable under BOTH JID schemes (aliased),
// plus an active GROUP whose chats.name carries that same human's name, and a
// vault where the person-named file is already occupied by the group with the
// newest last_message_ts.
func xvSeedFounderShape(t *testing.T, db *sql.DB, outDir string) {
	t.Helper()
	xvChat(t, db, xvLIDJID, "direct", xvPersonName, xvLIDLastTS)
	xvChat(t, db, xvPhoneJID, "direct", xvPersonName, xvPhoneLastTS)
	xvChat(t, db, xvGroupJID, "group", xvPersonName, xvGroupLastTS) // person-named group (the bridge.go bug)
	xvContact(t, db, xvLIDJID, xvPersonName, "")
	xvContact(t, db, xvPhoneJID, xvPersonName, "15555550100")
	xvAlias(t, db, xvLIDJID, xvPhoneJID)

	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, xvPersonName, "text", "hola por lid", "", xvLIDLastTS-100, false)
	xvMsg(t, db, "L2", xvLIDJID, xvLIDJID, xvPersonName, "voice", "", xvTranscript, xvLIDLastTS, false)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, xvPersonName, "text", "hola por telefono", "", xvPhoneLastTS-100, false)
	xvMsg(t, db, "P2", xvPhoneJID, "", "", "text", "respuesta mia", "", xvPhoneLastTS, true)

	xvMsg(t, db, "G1", xvGroupJID, "15555550111@s.whatsapp.net", "Member One", "text", "group chatter one", "", xvGroupLastTS-200, false)
	xvMsg(t, db, "G2", xvGroupJID, "15555550122@s.whatsapp.net", "Member Two", "text", "group chatter two", "", xvGroupLastTS-100, false)
	xvMsg(t, db, "G3", xvGroupJID, "31900125550033@lid", "Member Three", "text", "group chatter three", "", xvGroupLastTS, false)

	// The vault as the founder's machine had it: the person-named file holds
	// the GROUP, stamped with the group's (newest) last_message_ts.
	seed := strings.Join([]string{
		"---",
		"type: whatsapp-chat",
		fmt.Sprintf(`contact: "%s"`, xvPersonName),
		`phone: "+120363000000000001"`,
		fmt.Sprintf(`jid: "%s"`, xvGroupJID),
		`chat_type: "group"`,
		"message_count: 3",
		"first_message: 2026-07-25",
		"last_message: 2026-07-25",
		fmt.Sprintf("last_message_ts: %d", xvGroupLastTS),
		"last_sync: 2026-07-25",
		"participants_count: 0",
		"---",
		"",
		fmt.Sprintf("# WhatsApp: %s", xvPersonName),
		"",
		"## 2026-07-25",
		"",
		"**10:00 AM** Member One: group chatter one",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(outDir, xvPersonName+".md"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed vault file: %v", err)
	}
}

// TestExportDirectChatSurvivesGroupOccupyingItsFilename is the negative
// control for the whole ticket: a direct chat present in the DB with messages
// (including a transcribed voice note) must land in the vault even when a
// person-named GROUP file sits on its filename with a newer timestamp.
//
// Against the pre-fix exporter this fails: the group keeps the person-named
// file, the direct chat is "skipped-unchanged" against a file that was never
// its own, no file ever references either of the person's JIDs, and
// ExportVault returns nil.
func TestExportDirectChatSurvivesGroupOccupyingItsFilename(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	xvSeedFounderShape(t, db, outDir)

	if err := ExportVault(db, outDir, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	// 1. The person-named file must hold the person's DIRECT chat.
	personPath := filepath.Join(outDir, xvPersonName+".md")
	fm, body := xvReadFile(t, personPath)
	if fm["jid"] != xvLIDJID && fm["jid"] != xvPhoneJID {
		t.Fatalf("person-named file %q is not the person's direct chat: jid=%q (a group occupied a person-named file)", personPath, fm["jid"])
	}

	// 2. Both JID schemes merge into that ONE file: the voice note received
	// under the LID form and the text received under the phone form.
	if !strings.Contains(body, xvTranscript) {
		t.Errorf("direct chat file lost the LID-side voice note; body:\n%s", body)
	}
	if !strings.Contains(body, "hola por telefono") {
		t.Errorf("direct chat file lost the phone-side history; body:\n%s", body)
	}
	if fm["message_count"] != "4" {
		t.Errorf("message_count = %q, want 4 (both schemes merged)", fm["message_count"])
	}

	// 3. The other scheme is recorded so either JID greps to this file.
	other := xvPhoneJID
	if fm["jid"] == xvPhoneJID {
		other = xvLIDJID
	}
	if !strings.Contains(fm["alias_jids"], other) {
		t.Errorf("alias_jids = %q, want it to include %q", fm["alias_jids"], other)
	}

	// 4. The group still exports, under a name that can never be mistaken for
	// a person, with a usable participants_count.
	groupFile := xvFileForJID(t, outDir, xvGroupJID)
	if groupFile == "" {
		t.Fatal("group chat lost: no file claims the group jid")
	}
	if !strings.Contains(groupFile, "(group") {
		t.Errorf("group file %q carries no group marker in its filename", groupFile)
	}
	gfm, _ := xvReadFile(t, filepath.Join(outDir, groupFile))
	if n, _ := strconv.Atoi(gfm["participants_count"]); n < 3 {
		t.Errorf("group participants_count = %q, want >= 3 (distinct senders floor)", gfm["participants_count"])
	}
}

// TestExportMergesBothJIDSchemesIntoOneFile isolates defect 3: without a
// colliding group, the LID and phone rows of the same human must still merge
// into exactly one file holding BOTH histories. Pre-fix, the two rows race for
// one path and the survivor holds only its own scheme's half.
func TestExportMergesBothJIDSchemesIntoOneFile(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const person = "Merged Person"
	xvChat(t, db, xvLIDJID, "direct", person, xvLIDLastTS)
	xvChat(t, db, xvPhoneJID, "direct", person, xvPhoneLastTS)
	xvContact(t, db, xvLIDJID, person, "")
	xvContact(t, db, xvPhoneJID, person, "15555550100")
	xvAlias(t, db, xvLIDJID, xvPhoneJID)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, person, "text", "mensaje por lid", "", xvLIDLastTS, false)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, person, "text", "mensaje por telefono", "", xvPhoneLastTS, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	var mdFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	if len(mdFiles) != 1 {
		t.Fatalf("want exactly 1 file for one human, got %d: %v", len(mdFiles), mdFiles)
	}
	_, body := xvReadFile(t, filepath.Join(outDir, mdFiles[0]))
	if !strings.Contains(body, "mensaje por lid") || !strings.Contains(body, "mensaje por telefono") {
		t.Errorf("merged file must hold both schemes' history; body:\n%s", body)
	}
}

// TestExportMinMessagesCountsAcrossSchemes: a human whose history is split
// 1+1 across the two JID forms passes a min-messages threshold of 2. Pre-fix
// the filter ran per JID row, so a split history fell under the threshold on
// both rows and the whole human silently vanished from the export.
func TestExportMinMessagesCountsAcrossSchemes(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const person = "Split History"
	xvChat(t, db, xvLIDJID, "direct", person, xvLIDLastTS)
	xvChat(t, db, xvPhoneJID, "direct", person, xvPhoneLastTS)
	xvContact(t, db, xvLIDJID, person, "")
	xvContact(t, db, xvPhoneJID, person, "15555550100")
	xvAlias(t, db, xvLIDJID, xvPhoneJID)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, person, "text", "primera mitad", "", xvLIDLastTS, false)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, person, "text", "segunda mitad", "", xvPhoneLastTS, false)

	if err := ExportVault(db, outDir, false, 2); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			found = true
		}
	}
	if !found {
		t.Fatal("human with 2 messages split across JID schemes was dropped by min-messages=2")
	}
}

// TestExportGroupNeverTakesAPersonNamedFile: a group whose stored name is a
// person's name (the legacy bridge.go behavior) must export under a filename
// that is marked as a group, even when no direct chat competes for the name.
func TestExportGroupNeverTakesAPersonNamedFile(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	xvChat(t, db, xvGroupJID, "group", "Maria Team", xvGroupLastTS)
	xvMsg(t, db, "G1", xvGroupJID, "15555550111@s.whatsapp.net", "Member One", "text", "hello team", "", xvGroupLastTS, false)

	if err := ExportVault(db, outDir, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	groupFile := xvFileForJID(t, outDir, xvGroupJID)
	if groupFile == "" {
		t.Fatal("group chat produced no file")
	}
	if !strings.Contains(groupFile, "(group") {
		t.Errorf("group file %q must carry a group marker in its filename", groupFile)
	}
	if _, err := os.Stat(filepath.Join(outDir, "Maria Team.md")); err == nil {
		t.Error("bare person-shaped filename exists for a group chat")
	}
}

// TestExportFailsLoudWhenAChatCannotBeWritten: a chat that the exporter
// enumerates but cannot write must make the run FAIL and be NAMED. Pre-fix the
// error was logged, counted as a skip, and the run exited 0 — the
// SILENT-NO-OP-ON-UNEVALUABLE-SPEC class this ticket exists to kill.
func TestExportFailsLoudWhenAChatCannotBeWritten(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const person = "Blocked Person"
	const jid = "15555550199@s.whatsapp.net"
	xvChat(t, db, jid, "direct", person, xvPhoneLastTS)
	xvContact(t, db, jid, person, "15555550199")
	xvMsg(t, db, "B1", jid, jid, person, "text", "no me pierdas", "", xvPhoneLastTS, false)

	// Occupy the chat's target path with a DIRECTORY so the write must fail.
	if err := os.MkdirAll(filepath.Join(outDir, person+".md"), 0o755); err != nil {
		t.Fatalf("occupy path: %v", err)
	}

	err := ExportVault(db, outDir, false, 0)
	if err == nil {
		t.Fatal("ExportVault returned nil while a chat with messages produced no file (silent drop, rc=0)")
	}
	if !strings.Contains(err.Error(), jid) {
		t.Errorf("the error must NAME the dropped chat %q; got: %v", jid, err)
	}
}
