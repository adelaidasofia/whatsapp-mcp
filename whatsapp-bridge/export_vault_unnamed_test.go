package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tickets#6939: a chat with no known name must not be filed under its phone
// number or JID. Studio shows the file name as the document title, so those
// chats read as bare numbers. Fixture identities are synthetic (555 numbers,
// made-up LIDs).

func xvHasDigitsOf(name, jid string) bool {
	d := extractPhone(jid)
	return d != "" && (strings.Contains(name, d) || strings.Contains(name, d[:6]))
}

func TestExportNamesUnknownChatsWithoutTheirNumber(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	const otherPhoneJID = "15555550199@s.whatsapp.net"
	// Phone-only direct chat, LID-only direct chat, a second phone-only chat
	// (must not share a file), and a group with no subject.
	xvChat(t, db, xvPhoneJID, "direct", "", xvPhoneLastTS)
	xvChat(t, db, otherPhoneJID, "direct", "+1 555 555 0199", xvPhoneLastTS-1)
	xvChat(t, db, xvLIDJID, "direct", "", xvLIDLastTS)
	xvChat(t, db, xvGroupJID, "group", "", xvGroupLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, "", "text", "hola", "", xvPhoneLastTS, false)
	xvMsg(t, db, "O1", otherPhoneJID, otherPhoneJID, "", "text", "hey", "", xvPhoneLastTS-1, false)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "", "text", "buenas", "", xvLIDLastTS, false)
	xvMsg(t, db, "G1", xvGroupJID, xvPhoneJID, "", "text", "grupo", "", xvGroupLastTS, false)

	if err := ExportVault(db, outDir, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	seen := map[string]bool{}
	for _, jid := range []string{xvPhoneJID, otherPhoneJID, xvLIDJID, xvGroupJID} {
		name := xvFileForJID(t, outDir, jid)
		if name == "" {
			t.Fatalf("no file for %s", jid)
		}
		if seen[name] {
			t.Fatalf("two unknown chats share %q", name)
		}
		seen[name] = true
		for _, other := range []string{xvPhoneJID, otherPhoneJID, xvLIDJID, xvGroupJID} {
			if xvHasDigitsOf(name, other) {
				t.Errorf("file name %q carries a chat's number", name)
			}
		}
		fm, body := xvReadFile(t, filepath.Join(outDir, name))
		if fm["jid"] != jid {
			t.Errorf("%q: frontmatter jid = %q, want %q (identifier stays in frontmatter)", name, fm["jid"], jid)
		}
		if strings.HasPrefix(jid, "1555") && fm["phone"] == "" {
			t.Errorf("%q: frontmatter phone dropped", name)
		}
		if jid == xvGroupJID {
			if !strings.HasPrefix(name, "Group chat (WhatsApp) ") || !strings.HasSuffix(name, " (group).md") {
				t.Errorf("group file = %q, want \"Group chat (WhatsApp) <id> (group).md\"", name)
			}
		} else if !strings.HasPrefix(name, "Chat with an unknown number (WhatsApp) ") {
			t.Errorf("direct file = %q, want \"Chat with an unknown number (WhatsApp) <id>.md\"", name)
		}
		if strings.Contains(strings.SplitN(body, "\n## ", 2)[0], extractPhone(jid)) {
			t.Errorf("%q: heading carries the number", name)
		}
	}

	// A re-export lands on the same files: nothing new, nothing renamed.
	if err := ExportVault(db, outDir, true, 0); err != nil {
		t.Fatalf("second ExportVault: %v", err)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != len(seen) {
		t.Errorf("re-export changed the file set: %d files, want %d", len(entries), len(seen))
	}
	for name := range seen {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("re-export moved %q: %v", name, err)
		}
	}
}

// An old file named after the bare number is renamed on the next export, and
// once the person's name is known the unknown-number file is renamed to it:
// both through the frontmatter jid, so no chat is duplicated.
func TestExportRenamesNumberFileThenNamedFile(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	xvChat(t, db, xvPhoneJID, "direct", "", xvPhoneLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, "", "text", "hola", "", xvPhoneLastTS, false)

	legacy := "+" + extractPhone(xvPhoneJID) + ".md"
	seed := "---\ntype: whatsapp-chat\ncontact: \"+" + extractPhone(xvPhoneJID) + "\"\njid: \"" + xvPhoneJID + "\"\nchat_type: \"direct\"\nlast_message_ts: 1\n---\n\nold\n"
	if err := os.WriteFile(filepath.Join(outDir, legacy), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, legacy)); !os.IsNotExist(err) {
		t.Errorf("number-named file %q still present (err=%v)", legacy, err)
	}
	unknown := xvFileForJID(t, outDir, xvPhoneJID)
	if !strings.HasPrefix(unknown, "Chat with an unknown number (WhatsApp) ") {
		t.Fatalf("file after first export = %q", unknown)
	}

	xvContact(t, db, xvPhoneJID, xvPersonName, extractPhone(xvPhoneJID))
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("second ExportVault: %v", err)
	}
	if got := xvFileForJID(t, outDir, xvPhoneJID); got != xvPersonName+".md" {
		t.Errorf("file once the name is known = %q, want %q", got, xvPersonName+".md")
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		t.Errorf("chat duplicated across renames: %d files", len(entries))
	}
}

func TestLooksLikeIdentifier(t *testing.T) {
	for in, want := range map[string]bool{
		"+1 555 555 0199":    true,
		"15555550199":        true,
		"(555) 555-0199":     true,
		"84930125550023@lid": true,
		"Alex Rivera":        false,
		"Familia 2026":       false,
		"":                   false,
	} {
		if got := looksLikeIdentifier(in); got != want {
			t.Errorf("looksLikeIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
}

// The other side of an unknown chat is labelled with the readable label in
// the message lines too, never with the number.
func TestExportLabelsUnknownSenderWithoutTheNumber(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	xvChat(t, db, xvPhoneJID, "direct", "", xvPhoneLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, "", "text", "hola", "", xvPhoneLastTS, false)
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	_, body := xvReadFile(t, filepath.Join(outDir, xvFileForJID(t, outDir, xvPhoneJID)))
	if strings.Contains(body, extractPhone(xvPhoneJID)) {
		t.Errorf("body carries the number:\n%s", body)
	}
	if !strings.Contains(body, "Chat with an unknown number: hola") {
		t.Errorf("message line not labelled readably:\n%s", body)
	}
}

// An unknown chat that later merges into a person who already has a named
// file ends with one file: the named one holds every message and the old
// unknown-number file is retired.
func TestExportRetiresUnknownFileAbsorbedByNamedPerson(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	xvChat(t, db, xvPhoneJID, "direct", xvPersonName, xvPhoneLastTS)
	xvContact(t, db, xvPhoneJID, xvPersonName, extractPhone(xvPhoneJID))
	xvChat(t, db, xvLIDJID, "direct", "", xvLIDLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, xvPersonName, "text", "desde el telefono", "", xvPhoneLastTS, false)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "", "text", "desde el lid", "", xvLIDLastTS, false)

	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 2 {
		t.Fatalf("before the merge: %d files, want 2", len(entries))
	}

	xvAlias(t, db, xvLIDJID, xvPhoneJID)
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("second ExportVault: %v", err)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 || entries[0].Name() != xvPersonName+".md" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("after the merge: %v, want only %q", names, xvPersonName+".md")
	}
	_, body := xvReadFile(t, filepath.Join(outDir, entries[0].Name()))
	for _, want := range []string{"desde el telefono", "desde el lid"} {
		if !strings.Contains(body, want) {
			t.Errorf("merged file lost %q:\n%s", want, body)
		}
	}
}

// A leftover file holding more messages than the database still has is kept.
func TestExportKeepsAbsorbedFileTheDatabaseNoLongerCovers(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	xvChat(t, db, xvPhoneJID, "direct", xvPersonName, xvPhoneLastTS)
	xvContact(t, db, xvPhoneJID, xvPersonName, extractPhone(xvPhoneJID))
	xvChat(t, db, xvLIDJID, "direct", "", xvLIDLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, xvPersonName, "text", "uno", "", xvPhoneLastTS, false)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "", "text", "dos", "", xvLIDLastTS, false)
	xvAlias(t, db, xvLIDJID, xvPhoneJID)
	old := "Chat with an unknown number (WhatsApp) old.md"
	seed := "---\ntype: whatsapp-chat\njid: \"" + xvLIDJID + "\"\nchat_type: \"direct\"\nmessage_count: 40\n---\n\nolder history\n"
	if err := os.WriteFile(filepath.Join(outDir, xvPersonName+".md"), []byte("---\ntype: whatsapp-chat\njid: \""+xvPhoneJID+"\"\nchat_type: \"direct\"\nmessage_count: 1\n---\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, old), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExportVault(db, outDir, false, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, old)); err != nil {
		t.Errorf("file with history the database lacks was removed: %v", err)
	}
}

// The bridge stores the number or the JID as sender_display when it knows no
// name. Neither reaches the message lines.
func TestExportLabelsNumberShapedSenderDisplays(t *testing.T) {
	db := xvDB(t)
	outDir := t.TempDir()
	const memberJID = "15555550177@s.whatsapp.net"
	xvChat(t, db, xvPhoneJID, "direct", "", xvPhoneLastTS)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, "+"+extractPhone(xvPhoneJID), "text", "numero", "", xvPhoneLastTS, false)
	xvChat(t, db, xvGroupJID, "group", "Familia", xvGroupLastTS)
	xvMsg(t, db, "G1", xvGroupJID, memberJID, "+"+extractPhone(memberJID), "text", "por numero", "", xvGroupLastTS, false)
	xvMsg(t, db, "G2", xvGroupJID, xvLIDJID, xvLIDJID, "text", "por jid", "", xvGroupLastTS+1, false)
	if err := ExportVault(db, outDir, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	_, direct := xvReadFile(t, filepath.Join(outDir, xvFileForJID(t, outDir, xvPhoneJID)))
	if !strings.Contains(direct, "Chat with an unknown number: numero") {
		t.Errorf("direct line not labelled readably:\n%s", direct)
	}
	_, group := xvReadFile(t, filepath.Join(outDir, xvFileForJID(t, outDir, xvGroupJID)))
	for _, jid := range []string{xvPhoneJID, memberJID, xvLIDJID} {
		if strings.Contains(direct+group, extractPhone(jid)) {
			t.Errorf("a number reached the text:\n%s\n%s", direct, group)
		}
	}
	if strings.Count(group, "Unknown member ") != 2 {
		t.Errorf("unknown group members not labelled:\n%s", group)
	}
	lines := strings.Split(group, "\n")
	var a, b string
	for _, l := range lines {
		if strings.Contains(l, "por numero") {
			a = strings.SplitN(l, ": ", 2)[0]
		}
		if strings.Contains(l, "por jid") {
			b = strings.SplitN(l, ": ", 2)[0]
		}
	}
	if a == "" || b == "" || a[strings.Index(a, "Unknown"):] == b[strings.Index(b, "Unknown"):] {
		t.Errorf("two unknown members share one label: %q / %q", a, b)
	}
}
