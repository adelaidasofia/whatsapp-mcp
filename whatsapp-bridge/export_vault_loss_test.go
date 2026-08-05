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
		// Ownership is type-aware (round 5, F1): a user note carrying a chat
		// jid is not that chat's file, and the helper must judge coverage the
		// way the exporter and reconcile do.
		if fm["type"] == "whatsapp-chat" && fm["jid"] == jid {
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

// TestExportDisambiguatesSameNameSamePhoneWithoutAliasEdge — PR #64 review,
// HIGH finding, executed counterexample (a): the SAME human's LID and phone
// rows with NO jid_aliases edge (alias coverage is incomplete in the wild —
// Baileys-imported contacts carry phones but the import never writes edges).
// The LID contact carries the repaired real phone, so BOTH units display the
// same name AND disambiguate to the same "+phone" tag. First-round collision
// handling keyed on the undecorated name never re-checked final names, so
// both units silently shared one path: one file, one chat gone, rc=0.
func TestExportDisambiguatesSameNameSamePhoneWithoutAliasEdge(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const person = "Same Name"
	xvChat(t, db, xvLIDJID, "direct", person, xvLIDLastTS)
	xvChat(t, db, xvPhoneJID, "direct", person, xvPhoneLastTS)
	// The LID row carries the repaired REAL phone; deliberately NO alias edge.
	xvContact(t, db, xvLIDJID, person, "15555550100")
	xvContact(t, db, xvPhoneJID, person, "15555550100")
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, person, "text", "lado lid", "", xvLIDLastTS, false)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, person, "text", "lado telefono", "", xvPhoneLastTS, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	lidFile := xvFileForJID(t, outDir, xvLIDJID)
	phoneFile := xvFileForJID(t, outDir, xvPhoneJID)
	if lidFile == "" || phoneFile == "" {
		t.Fatalf("a chat lost its file to a residual filename collision: lid=%q phone=%q", lidFile, phoneFile)
	}
	if lidFile == phoneFile {
		t.Fatalf("both chats claim one file %q — the collision was not resolved", lidFile)
	}
}

// TestExportPushNameMimicsDisambiguatedName — PR #64 review, HIGH finding,
// executed counterexample (b): push names are attacker-controlled, and a push
// name that LOOKS like a disambiguated name ("Same Name (+15555550311)")
// collided with the genuinely disambiguated file of a different human named
// "Same Name" with phone 15555550311. Final names must be checked to a fixed
// point, not derived in one pass from the undecorated name.
func TestExportPushNameMimicsDisambiguatedName(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const mimicJID = "15555550377@s.whatsapp.net"
	const victimAJID = "15555550311@s.whatsapp.net"
	const victimBJID = "15555550322@s.whatsapp.net"
	xvChat(t, db, mimicJID, "direct", "Same Name (+15555550311)", xvPhoneLastTS)
	xvChat(t, db, victimAJID, "direct", "Same Name", xvPhoneLastTS-10)
	xvChat(t, db, victimBJID, "direct", "Same Name", xvPhoneLastTS-20)
	xvContact(t, db, mimicJID, "Same Name (+15555550311)", "15555550377")
	xvContact(t, db, victimAJID, "Same Name", "15555550311")
	xvContact(t, db, victimBJID, "Same Name", "15555550322")
	xvMsg(t, db, "M1", mimicJID, mimicJID, "Same Name (+15555550311)", "text", "soy quien digo ser", "", xvPhoneLastTS, false)
	xvMsg(t, db, "A1", victimAJID, victimAJID, "Same Name", "text", "soy a", "", xvPhoneLastTS-10, false)
	xvMsg(t, db, "B1", victimBJID, victimBJID, "Same Name", "text", "soy b", "", xvPhoneLastTS-20, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	seen := map[string]string{}
	for _, jid := range []string{mimicJID, victimAJID, victimBJID} {
		f := xvFileForJID(t, outDir, jid)
		if f == "" {
			t.Fatalf("chat %s lost its file to a push name mimicking a disambiguated name", jid)
		}
		if owner, dup := seen[f]; dup {
			t.Fatalf("chats %s and %s share one file %q", owner, jid, f)
		}
		seen[f] = jid
	}
}

// TestExportFailsLoudOnUnresolvableFilenameCollision: when even the JID-based
// suffixes cannot separate two chats (pathological JIDs that sanitize to the
// same filename), the export must refuse to run rather than let two units
// race for one path with rc=0. Fail closed, name both chats.
func TestExportFailsLoudOnUnresolvableFilenameCollision(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	// Distinct JIDs whose sanitized forms are identical (":" sanitizes to "-")
	// and which contain no digits, so every disambiguation level collides.
	const jidA = "a:b@lid"
	const jidB = "a-b@lid"
	xvChat(t, db, jidA, "direct", "Weird Pair", xvPhoneLastTS)
	xvChat(t, db, jidB, "direct", "Weird Pair", xvPhoneLastTS-10)
	xvMsg(t, db, "W1", jidA, jidA, "Weird Pair", "text", "uno", "", xvPhoneLastTS, false)
	xvMsg(t, db, "W2", jidB, jidB, "Weird Pair", "text", "dos", "", xvPhoneLastTS-10, false)

	err := ExportVault(db, outDir, false, 0)
	if err == nil {
		t.Fatal("ExportVault returned nil while two chats contend for one filename")
	}
	if !strings.Contains(err.Error(), jidA) || !strings.Contains(err.Error(), jidB) {
		t.Errorf("the error must NAME both colliding chats; got: %v", err)
	}
	// Positive control on the branch itself (PR #64 round 4, finding 3): this
	// failure must come from the fixed-point RESIDUE gate, not from some other
	// error path that happens to mention both JIDs. If the escalation ever
	// starts resolving this fixture, this assertion goes red and the residue
	// backstop is known to have lost its only coverage.
	if !strings.Contains(err.Error(), "filename collision after disambiguation") {
		t.Errorf("expected the residue fail-closed gate to fire; got: %v", err)
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

// TestExportSeparatesNormalizationTwinNames — PR #64 review round 3, HIGH,
// executed at dd54464: the dedup key and the final-uniqueness gate compared
// STRING identity, but APFS (macOS default, where this bridge ships and the
// vault is iCloud-synced) resolves filenames normalization-INSENSITIVELY. Two
// different humans whose push names are the NFC and NFD spellings of "José"
// are byte-distinct — no collision detected, neither escalates — yet both
// units open ONE physical file, racing the same inode with rc=0. Real
// reachability: accented names are the norm in this product's LatAm market,
// mixed NFC/NFD arrives from real devices and vCard imports, and a contact
// can deliberately set the NFD twin of another contact's name (the same
// mimicry class as the round-2 fix, one axis deeper).
//
// After the fix, filenames are NFC-folded before comparison AND before
// writing, so the twins collide at plan time and escalate to distinct
// "+phone" names. On byte-literal filesystems (ext4) the pre-fix code
// happened to write two files, so the RED proof for this test is on macOS;
// post-fix behavior is identical on every platform.
func TestExportSeparatesNormalizationTwinNames(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const nfcJID = "15555550201@s.whatsapp.net"
	const nfdJID = "15555550202@s.whatsapp.net"
	nameNFC := "José"  // é precomposed
	nameNFD := "José" // e + combining acute
	if nameNFC == nameNFD {
		t.Fatal("fixture broken: the two spellings must be byte-distinct")
	}
	xvChat(t, db, nfcJID, "direct", nameNFC, xvPhoneLastTS)
	xvChat(t, db, nfdJID, "direct", nameNFD, xvPhoneLastTS-10)
	xvContact(t, db, nfcJID, nameNFC, "15555550201")
	xvContact(t, db, nfdJID, nameNFD, "15555550202")
	xvMsg(t, db, "N1", nfcJID, nfcJID, nameNFC, "text", "soy nfc", "", xvPhoneLastTS, false)
	xvMsg(t, db, "N2", nfdJID, nfdJID, nameNFD, "text", "soy nfd", "", xvPhoneLastTS-10, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	nfcFile := xvFileForJID(t, outDir, nfcJID)
	nfdFile := xvFileForJID(t, outDir, nfdJID)
	if nfcFile == "" || nfdFile == "" {
		t.Fatalf("a chat lost its file to filename normalization aliasing: nfc=%q nfd=%q", nfcFile, nfdFile)
	}
	if nfcFile == nfdFile {
		t.Fatalf("both chats claim one file %q", nfcFile)
	}
}

// TestExportResolvesLongMimicChains — PR #64 review round 3, MEDIUM, executed
// at dd54464: the escalation loop was capped at 3 rounds, not run to its
// fixed point. This 5-chat corpus ("Bob" twice, plus push names mimicking
// each successive decorated form) needs more rounds than the cap; the capped
// loop failed CLOSED (residue error, zero files) — no data loss, but four
// attacker-influenceable push names bricked the entire export until someone
// renamed a contact. The unbounded loop provably terminates (each continuing
// round escalates at least one unit and levels are capped, so total
// escalations are bounded by 2n) and separates this corpus in five rounds.
func TestExportResolvesLongMimicChains(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	chain := []struct {
		jid   string
		name  string
		phone string
	}{
		{"15555550411@s.whatsapp.net", "Bob", "15555550411"},
		{"15555550422@s.whatsapp.net", "Bob", "15555550422"},
		{"15555550433@s.whatsapp.net", "Bob (+15555550411)", "15555550433"},
		{"15555550444@s.whatsapp.net", "Bob (+15555550411) (+15555550433)", "15555550444"},
		{"15555550455@s.whatsapp.net", "Bob (+15555550411) (+15555550433) (+15555550444)", "15555550455"},
	}
	for i, c := range chain {
		xvChat(t, db, c.jid, "direct", c.name, xvPhoneLastTS-int64(i))
		xvContact(t, db, c.jid, c.name, c.phone)
		xvMsg(t, db, fmt.Sprintf("C%d", i), c.jid, c.jid, c.name, "text", "hola", "", xvPhoneLastTS-int64(i), false)
	}

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault must separate a resolvable mimic chain, got: %v", err)
	}

	seen := map[string]string{}
	for _, c := range chain {
		f := xvFileForJID(t, outDir, c.jid)
		if f == "" {
			t.Fatalf("chat %s got no file", c.jid)
		}
		if owner, dup := seen[f]; dup {
			t.Fatalf("chats %s and %s share one file %q", owner, c.jid, f)
		}
		seen[f] = c.jid
	}
}

// TestExportDetectsFilesystemAliasedPaths — PR #64 review round 3, HIGH part
// (b): name-level prevention (NFC folding, case-insensitive dedup) can only
// cover the aliasing axes we know about. The reconciliation must verify the
// RECIPIENT's predicate — distinct units landed on distinct FILES — via
// os.SameFile, so any filesystem mechanism that maps two distinct names onto
// one inode (normalization, case-folding, links) fails the run loudly instead
// of counting two chats as written into one file. Here the aliasing is forced
// with a hard link, the axis no name folding can see.
func TestExportDetectsFilesystemAliasedPaths(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const jidA = "15555550401@s.whatsapp.net"
	const jidB = "15555550402@s.whatsapp.net"
	xvChat(t, db, jidA, "direct", "Person A", xvPhoneLastTS)
	xvChat(t, db, jidB, "direct", "Person B", xvPhoneLastTS-10)
	xvContact(t, db, jidA, "Person A", "15555550401")
	xvContact(t, db, jidB, "Person B", "15555550402")
	xvMsg(t, db, "H1", jidA, jidA, "Person A", "text", "soy a", "", xvPhoneLastTS, false)
	xvMsg(t, db, "H2", jidB, jidB, "Person B", "text", "soy b", "", xvPhoneLastTS-10, false)

	// Alias the two (collision-free) target paths onto ONE inode. The seeded
	// file carries B's jid so both dir entries stay INSIDE this run's
	// population (since round 4, a jid-less or foreign file would be treated
	// as an obstacle and the units would escalate their names away from it —
	// this test is about the aliasing axes name planning cannot see).
	seed := "---\ntype: whatsapp-chat\njid: \"" + jidB + "\"\nchat_type: \"direct\"\nlast_message_ts: 1\n---\n\nold\n"
	if err := os.WriteFile(filepath.Join(outDir, "Person B.md"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed alias target: %v", err)
	}
	if err := os.Link(filepath.Join(outDir, "Person B.md"), filepath.Join(outDir, "Person A.md")); err != nil {
		t.Skipf("filesystem does not support hard links here: %v", err)
	}

	err := ExportVault(db, outDir, false, 0)
	if err == nil {
		t.Fatal("ExportVault returned nil while two chats' files are one inode (a write raced and one history was destroyed)")
	}
	if !strings.Contains(err.Error(), jidA) || !strings.Contains(err.Error(), jidB) {
		t.Errorf("the error must NAME both aliased chats; got: %v", err)
	}
}

// TestExportPreservesFilteredOutChatFileOnCaseStraddle — PR #64 review round
// 4, HIGH, executed at 3ef9e80 (reviewer probe, ported): the SameFile sweep
// measured THIS RUN'S UNITS only, so a file belonging to a chat OUTSIDE the
// unit set — filtered by min-messages, excluded by includeGroups=false (the
// default), or deleted from the DB since a previous export — was never
// statted. Chat B ("Jose", 2 msgs) was exported under min-messages=0; later a
// DIFFERENT human ("JOSE", 6 msgs) exported under min-messages=5. On a
// case-insensitive volume (APFS/NTFS defaults) the write to "JOSE.md" opened
// B's "Jose.md": one dir entry left carrying A's jid, B's exported history
// destroyed, err=nil, dropped=0, and a matched-flags reconcile reported zero
// findings. Operating rule: A Measured Population Is a Floor Until the
// Measurement Itself Is Exhaustive — the guard's population must be the
// entire namespace the exporter can write through, not the unit subset.
//
// Post-fix: names are planned AGAINST the existing directory (a fold-equal
// entry claiming a jid outside the unit set escalates the unit's name), so
// both humans coexist on every filesystem and B's file survives byte-for-byte.
func TestExportPreservesFilteredOutChatFileOnCaseStraddle(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	// The straddle needs a case-insensitive volume; skip where names are
	// byte-literal (ext4 CI leg) — behavior there was already two files.
	if err := os.WriteFile(filepath.Join(outDir, "probe_case.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, caseErr := os.Stat(filepath.Join(outDir, "PROBE_CASE.md"))
	os.Remove(filepath.Join(outDir, "probe_case.md"))
	if caseErr != nil {
		t.Skip("volume is case-sensitive; straddle not applicable here")
	}

	const jidB = "15555550902@s.whatsapp.net"
	xvChat(t, db, jidB, "direct", "Jose", xvPhoneLastTS-100)
	xvContact(t, db, jidB, "Jose", "15555550902")
	xvMsg(t, db, "B1", jidB, jidB, "Jose", "text", "historia de jose b", "", xvPhoneLastTS-101, false)
	xvMsg(t, db, "B2", jidB, jidB, "Jose", "text", "mas historia b", "", xvPhoneLastTS-100, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "Jose.md")); err != nil {
		t.Fatalf("run 1 must produce Jose.md: %v", err)
	}

	const jidA = "15555550901@s.whatsapp.net"
	xvChat(t, db, jidA, "direct", "JOSE", xvPhoneLastTS)
	xvContact(t, db, jidA, "JOSE", "15555550901")
	for i, msg := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		xvMsg(t, db, "A"+msg, jidA, jidA, "JOSE", "text", "historia de JOSE "+msg, "", xvPhoneLastTS-int64(i), false)
	}

	// Run 2: narrow filter. B (2 msgs) is OUTSIDE the unit set; A is in.
	if err := ExportVault(db, outDir, false, 5); err != nil {
		t.Fatalf("run 2 must succeed by escalating A's name away from B's file, got: %v", err)
	}

	bFile := xvFileForJID(t, outDir, jidB)
	if bFile == "" {
		t.Fatal("filtered-out chat B's exported file was destroyed by a case-straddling unit write")
	}
	_, bBody := xvReadFile(t, filepath.Join(outDir, bFile))
	if !strings.Contains(bBody, "historia de jose b") {
		t.Errorf("B's file survived in name but lost its history:\n%s", bBody)
	}
	aFile := xvFileForJID(t, outDir, jidA)
	if aFile == "" {
		t.Fatal("chat A was not exported")
	}
	if aFile == bFile {
		t.Fatalf("A and B share one file %q", aFile)
	}
	// The matched-flags reconcile must agree the vault is complete.
	findings, err := ReconcileVault(db, outDir, false, 5)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("reconcile findings on a healthy straddled vault: %v", findings)
	}
}

// TestExportFoldsDisambiguationTagsThroughSanitizer — PR #64 review round 4,
// finding 2: the direct level-1 disambiguation tag interpolated u.phone
// VERBATIM, bypassing the sanitizeFilename NFC chokepoint the round-3 comment
// claimed covered "dedup keys, final names, and disambiguation tags". Two
// LID-only contacts sharing a push name whose contacts.phone values are
// NFC/NFD twins produced byte-distinct finals that alias on APFS — the
// SameFile sweep caught it (fail closed, no data loss) but BRICKED an export
// that should have succeeded. Post-fix the tag routes through
// sanitizeFilename: the twin tags fold equal, escalate to distinct level-2
// JIDs, and both chats export everywhere.
func TestExportFoldsDisambiguationTagsThroughSanitizer(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const jidX = "84930125550041@lid"
	const jidY = "84930125550042@lid"
	// Explicit escapes, not literals: editors and toolchains love to
	// re-normalize source text, which would silently collapse the fixture.
	phoneNFC := "0601555\u00e9"  // e-acute precomposed (NFC)
	phoneNFD := "0601555e\u0301" // e + combining acute (NFD)
	if phoneNFC == phoneNFD {
		t.Fatal("fixture broken: the two spellings must be byte-distinct")
	}
	xvChat(t, db, jidX, "direct", "Ana", xvPhoneLastTS)
	xvChat(t, db, jidY, "direct", "Ana", xvPhoneLastTS-10)
	xvContact(t, db, jidX, "Ana", phoneNFC)
	xvContact(t, db, jidY, "Ana", phoneNFD)
	xvMsg(t, db, "X1", jidX, jidX, "Ana", "text", "soy x", "", xvPhoneLastTS, false)
	xvMsg(t, db, "Y1", jidY, jidY, "Ana", "text", "soy y", "", xvPhoneLastTS-10, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault must fold the tags and separate the twins, got: %v", err)
	}
	xFile := xvFileForJID(t, outDir, jidX)
	yFile := xvFileForJID(t, outDir, jidY)
	if xFile == "" || yFile == "" {
		t.Fatalf("a chat lost its file: x=%q y=%q", xFile, yFile)
	}
	if xFile == yFile {
		t.Fatalf("both chats claim one file %q", xFile)
	}
}

// TestExportUpdatesLegacyNFDFileInPlace — reviewer probe (round 4, ported):
// pre-round-3 exports wrote unfolded display bytes, so a device supplying NFD
// produced an NFD-named file. Post-fix the unit's filename is NFC. On a
// normalization-insensitive volume the NFC path opens that same physical
// file, which is the ONE legitimate shape of "unit file aliases a
// differently-named entry" — the cross-population sweep must recognize it
// via the entry's pre-run jid and not flag it, exactly one file must remain,
// and reconcile must stay clean. On byte-literal volumes healing renames the
// NFD file instead; either way the assertions below hold.
func TestExportUpdatesLegacyNFDFileInPlace(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const jid = "15555550903@s.whatsapp.net"
	nameNFD := "José" // e + combining acute — what a pre-fix export wrote
	xvChat(t, db, jid, "direct", nameNFD, xvPhoneLastTS)
	xvContact(t, db, jid, nameNFD, "15555550903")
	xvMsg(t, db, "M1", jid, jid, nameNFD, "text", "hola", "", xvPhoneLastTS, false)

	legacyContent := "---\ntype: whatsapp-chat\njid: \"" + jid + "\"\nlast_message_ts: 1\nmessage_count: 1\n---\nold body\n"
	if err := os.WriteFile(filepath.Join(outDir, nameNFD+".md"), []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault flagged its own legacy file as a foreign alias: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var mds []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mds = append(mds, e.Name())
		}
	}
	if len(mds) != 1 {
		t.Fatalf("expected exactly one file after migration, got %v", mds)
	}
	fm, body := xvReadFile(t, filepath.Join(outDir, mds[0]))
	if fm["jid"] != jid || strings.Contains(body, "old body") {
		t.Fatalf("legacy file not regenerated in place: fm=%v", fm)
	}
	findings, err := ReconcileVault(db, outDir, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("reconcile findings on a migrated vault: %v", findings)
	}
}

// TestExportNeverDestroysUsersNoteCarryingAChatJID — PR #64 review round 5,
// F1 (HIGH), executed at 63c86fd and 3ef9e80: every ownership consumer read
// `.jid` alone and ignored `type:`, while reconcile's own definition of a
// chat file requires type: whatsapp-chat — the two halves disagreed about
// what the exporter owns. A user's note that carries a chat's jid in its
// frontmatter (realistic: duplicate a chat file to annotate it, the copy
// keeps the frontmatter; the contact's display name later changes) was
// heal-renamed onto the unit's canonical filename and force-rewritten — the
// user's own content destroyed, rc=0, reconcile clean. Ownership now
// requires jid membership AND the declared whatsapp-chat type; a
// jid-claiming non-chat file is an OBSTACLE, never a victim.
func TestExportNeverDestroysUsersNoteCarryingAChatJID(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	const jid = "15555550501@s.whatsapp.net"
	xvChat(t, db, jid, "direct", "Cliente Uno", xvPhoneLastTS)
	xvContact(t, db, jid, "Cliente Uno", "15555550501")
	xvMsg(t, db, "C1", jid, jid, "Cliente Uno", "text", "hola cliente", "", xvPhoneLastTS, false)

	// The user's own analysis note, annotated from a copy of the chat file —
	// it carries the chat's jid but is NOT a whatsapp-chat file.
	note := "---\ntype: analysis\njid: \"" + jid + "\"\nlast_message_ts: 1\n---\n\nMIS NOTAS IMPORTANTES\n"
	notePath := filepath.Join(outDir, "Anotaciones.md")
	if err := os.WriteFile(notePath, []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	data, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("the user's note was renamed or deleted by the export: %v", err)
	}
	if !strings.Contains(string(data), "MIS NOTAS IMPORTANTES") {
		t.Fatalf("the user's note content was destroyed:\n%s", data)
	}
	chatFile := xvFileForJID(t, outDir, jid)
	if chatFile == "" {
		t.Fatal("the chat itself was not exported")
	}
	if chatFile == "Anotaciones.md" {
		t.Fatal("the export claimed the user's note as the chat file")
	}
}

// TestExportNeverWritesThroughASymlink — PR #64 review round 5, F2
// (MEDIUM-HIGH), executed at 63c86fd and 3ef9e80: the planning scan used
// os.Stat and SKIPPED stat failures, so a DANGLING symlink squatting a
// unit's name was invisible — fail-open, inconsistent with the obstacle
// posture everywhere else — while os.WriteFile follows symlinks. The chat
// content was created OUTSIDE outputDir at the symlink's target, race-free,
// with `written=1 dropped=0`. The scan now uses os.Lstat: symlinks and
// unstattable entries are obstacles, so the unit's name escalates away and
// nothing is ever written through a link.
func TestExportNeverWritesThroughASymlink(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	leakTarget := filepath.Join(t.TempDir(), "leaked.md") // outside outputDir, nonexistent

	const jid = "15555550502@s.whatsapp.net"
	xvChat(t, db, jid, "direct", "Ana Lopez", xvPhoneLastTS)
	xvContact(t, db, jid, "Ana Lopez", "15555550502")
	xvMsg(t, db, "S1", jid, jid, "Ana Lopez", "text", "contenido privado", "", xvPhoneLastTS, false)

	if err := os.Symlink(leakTarget, filepath.Join(outDir, "Ana Lopez.md")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	if _, err := os.Stat(leakTarget); err == nil {
		t.Fatal("chat content escaped the output directory through a symlink")
	}
	chatFile := xvFileForJID(t, outDir, jid)
	if chatFile == "" {
		t.Fatal("the chat was not exported inside the output directory")
	}
	if fi, err := os.Lstat(filepath.Join(outDir, chatFile)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the exported chat file must be a real regular file (err=%v)", err)
	}
}

// TestExportRefusesCaseStraddleOntoAnotherUnitsStaleFile — PR #64 review
// round 5, F3: failure-path coverage for the round-4 pre-write guard and
// reconcile's COLLISION finding (a security control is tested on its own
// failure path, because that is where its regressions hide). The shape the
// obstacle planner cannot defuse: the victim file belongs to an
// IN-POPULATION unit (so it is not an obstacle), sits under a stale name
// healing cannot move (the unit's canonical file already exists), and a
// different unit's name case-straddles onto it. The guard must refuse that
// write NAMING both sides, the stale file must survive, and reconcile must
// predict the refusal as COLLISION.
func TestExportRefusesCaseStraddleOntoAnotherUnitsStaleFile(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()

	// Needs a case-insensitive volume for the straddle.
	if err := os.WriteFile(filepath.Join(outDir, "probe_case.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, caseErr := os.Stat(filepath.Join(outDir, "PROBE_CASE.md"))
	os.Remove(filepath.Join(outDir, "probe_case.md"))
	if caseErr != nil {
		t.Skip("volume is case-sensitive; straddle not applicable here")
	}

	const jidB = "15555550512@s.whatsapp.net" // renamed contact: old file + canonical file
	const jidA = "15555550511@s.whatsapp.net" // straddler whose display matches B's OLD filename
	xvChat(t, db, jidB, "direct", "Mari Nueva", xvPhoneLastTS)
	xvContact(t, db, jidB, "Mari Nueva", "15555550512")
	xvMsg(t, db, "B1", jidB, jidB, "Mari Nueva", "text", "historia b", "", xvPhoneLastTS, false)
	xvChat(t, db, jidA, "direct", "MARI OLD", xvPhoneLastTS-10)
	xvContact(t, db, jidA, "MARI OLD", "15555550511")
	xvMsg(t, db, "A1", jidA, jidA, "MARI OLD", "text", "historia a", "", xvPhoneLastTS-10, false)

	// B's stale pre-rename file (in-population jid, so NOT an obstacle) and
	// B's canonical file (so healing cannot move the stale one).
	stale := "---\ntype: whatsapp-chat\njid: \"" + jidB + "\"\nchat_type: \"direct\"\nlast_message_ts: 1\nmessage_count: 1\n---\n\nhistoria vieja de b\n"
	if err := os.WriteFile(filepath.Join(outDir, "Mari Old.md"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical := "---\ntype: whatsapp-chat\njid: \"" + jidB + "\"\nchat_type: \"direct\"\nlast_message_ts: " + fmt.Sprint(xvPhoneLastTS) + "\nmessage_count: 1\n---\n\nhistoria b\n"
	if err := os.WriteFile(filepath.Join(outDir, "Mari Nueva.md"), []byte(canonical), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reconcile FIRST: it must predict the refusal without mutating anything.
	findings, rerr := ReconcileVault(db, outDir, false, 0)
	if rerr != nil {
		t.Fatalf("reconcile: %v", rerr)
	}
	collision := false
	for _, f := range findings {
		if strings.HasPrefix(f, "COLLISION") && strings.Contains(f, jidA) {
			collision = true
		}
	}
	if !collision {
		t.Errorf("reconcile must predict the straddle as a COLLISION naming %s; findings: %v", jidA, findings)
	}

	err := ExportVault(db, outDir, false, 0)
	if err == nil {
		t.Fatal("the pre-write guard must refuse a write that resolves onto another unit's stale file")
	}
	if !strings.Contains(err.Error(), jidA) || !strings.Contains(err.Error(), "writing would destroy") || !strings.Contains(err.Error(), "Mari Old.md") {
		t.Errorf("the refusal must name the straddler and the victim file; got: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(outDir, "Mari Old.md"))
	if readErr != nil || !strings.Contains(string(data), "historia vieja de b") {
		t.Errorf("the stale file must survive the refused run intact (err=%v)", readErr)
	}
}
