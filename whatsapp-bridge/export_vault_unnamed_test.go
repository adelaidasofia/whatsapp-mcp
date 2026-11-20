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
