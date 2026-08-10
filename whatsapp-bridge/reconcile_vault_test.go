package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reconcile tool exists so the MYC-3555 failure classes are MEASURABLE
// against a live vault instead of discovered by hand-grepping 905 files.
// Fixtures reuse the synthetic identities from export_vault_loss_test.go.

func xvWriteChatFile(t *testing.T, dir, name string, fmLines []string, body string) {
	t.Helper()
	content := "---\n" + strings.Join(fmLines, "\n") + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func xvFindingCount(findings []string, prefix string) int {
	n := 0
	for _, f := range findings {
		if strings.HasPrefix(f, prefix) {
			n++
		}
	}
	return n
}

// TestReconcileFlagsEveryFailureClass builds a vault exhibiting all five
// defect classes at once and asserts each is individually reported.
func TestReconcileFlagsEveryFailureClass(t *testing.T) {
	db := xvDB(t)
	vault := t.TempDir()

	// MISSING: a direct chat with messages and no file anywhere.
	xvChat(t, db, xvLIDJID, "direct", "Lost Person", xvLIDLastTS)
	xvContact(t, db, xvLIDJID, "Lost Person", "")
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "Lost Person", "text", "donde estoy", "", xvLIDLastTS, false)

	// MISFILED + DRIFT: a group chat sitting in a person-named file whose
	// message_count also disagrees with the DB.
	xvChat(t, db, xvGroupJID, "group", "Team Chat", xvGroupLastTS)
	xvMsg(t, db, "G1", xvGroupJID, "15555550111@s.whatsapp.net", "Member One", "text", "uno", "", xvGroupLastTS-1, false)
	xvMsg(t, db, "G2", xvGroupJID, "15555550122@s.whatsapp.net", "Member Two", "text", "dos", "", xvGroupLastTS, false)
	xvWriteChatFile(t, vault, "Team Person.md", []string{
		"type: whatsapp-chat",
		`contact: "Team Person"`,
		fmt.Sprintf(`jid: "%s"`, xvGroupJID),
		`chat_type: "group"`,
		"message_count: 416",
		fmt.Sprintf("last_message_ts: %d", xvGroupLastTS),
	}, "old group content\n")

	// DUPLICATE: two files claiming the same direct chat.
	const dupJID = "15555550177@s.whatsapp.net"
	xvChat(t, db, dupJID, "direct", "Twice Filed", xvPhoneLastTS)
	xvMsg(t, db, "D1", dupJID, dupJID, "Twice Filed", "text", "hola", "", xvPhoneLastTS, false)
	for _, name := range []string{"Twice Filed.md", "+15555550177.md"} {
		xvWriteChatFile(t, vault, name, []string{
			"type: whatsapp-chat",
			`contact: "Twice Filed"`,
			fmt.Sprintf(`jid: "%s"`, dupJID),
			`chat_type: "direct"`,
			"message_count: 1",
			fmt.Sprintf("last_message_ts: %d", xvPhoneLastTS),
		}, "hola\n")
	}

	// ORPHAN: a chat file whose JID is gone from the DB.
	xvWriteChatFile(t, vault, "Ghost.md", []string{
		"type: whatsapp-chat",
		`contact: "Ghost"`,
		`jid: "15555550188@s.whatsapp.net"`,
		`chat_type: "direct"`,
		"message_count: 2",
	}, "adios\n")

	// A non-chat vault note must be ignored, not reported.
	if err := os.WriteFile(filepath.Join(vault, "Meeting Notes.md"), []byte("---\ntype: meeting\n---\nnotes"), 0o644); err != nil {
		t.Fatalf("write non-chat note: %v", err)
	}

	findings, err := ReconcileVault(db, vault, true, 0)
	if err != nil {
		t.Fatalf("ReconcileVault: %v", err)
	}

	for prefix, want := range map[string]int{
		"MISSING":   1,
		"MISFILED":  1,
		"DRIFT":     1, // the person-named group file records 416 messages, the DB holds 2
		"DUPLICATE": 1,
		"ORPHAN":    1,
	} {
		if got := xvFindingCount(findings, prefix); got != want {
			t.Errorf("%s findings = %d, want %d; all findings:\n%s", prefix, got, want, strings.Join(findings, "\n"))
		}
	}
}

// TestReconcileCleanAfterExport is the round-trip invariant: whatever
// ExportVault writes, ReconcileVault must accept without findings. If these
// two ever disagree about what complete means, one of them is lying.
//
// The zero-renderable chat is the sharp edge: rows exist (msgCount > 0) but
// every one is a legacy empty system row, so the export legitimately writes
// no file — and reconcile must judge that by the same renderability rule
// rather than flagging it MISSING.
func TestReconcileCleanAfterExport(t *testing.T) {
	db := xvDB(t)
	vault := t.TempDir()
	xvSeedFounderShape(t, db, vault)

	const emptyJID = "15555550166@s.whatsapp.net"
	xvChat(t, db, emptyJID, "direct", "Empty Rows", xvPhoneLastTS)
	xvMsg(t, db, "E1", emptyJID, emptyJID, "Empty Rows", "system", "", "", xvPhoneLastTS, false)

	if err := ExportVault(db, vault, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	findings, err := ReconcileVault(db, vault, true, 0)
	if err != nil {
		t.Fatalf("ReconcileVault: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("export → reconcile must be clean, got %d findings:\n%s", len(findings), strings.Join(findings, "\n"))
	}
}
