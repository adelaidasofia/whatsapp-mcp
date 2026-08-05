package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Planner-level coverage for MYC-3555: unit merging, deterministic collision
// naming, file-identity parsing, and legacy-filename healing.

func TestBuildExportUnitsMergesAliasedDirects(t *testing.T) {
	db := xvDB(t)
	xvChat(t, db, xvLIDJID, "direct", "Alex Rivera", xvLIDLastTS)
	xvChat(t, db, xvPhoneJID, "direct", "Alex Rivera", xvPhoneLastTS)
	xvContact(t, db, xvLIDJID, "Alex Rivera", "")
	xvContact(t, db, xvPhoneJID, "Alex Rivera", "15555550100")
	xvAlias(t, db, xvLIDJID, xvPhoneJID)
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "Alex Rivera", "text", "uno", "", xvLIDLastTS, false)
	xvMsg(t, db, "P1", xvPhoneJID, xvPhoneJID, "Alex Rivera", "text", "dos", "", xvPhoneLastTS, false)
	xvMsg(t, db, "P2", xvPhoneJID, xvPhoneJID, "Alex Rivera", "text", "tres", "", xvPhoneLastTS-1, false)

	units, total, err := buildExportUnits(db, false, 0)
	if err != nil {
		t.Fatalf("buildExportUnits: %v", err)
	}
	if total != 2 {
		t.Errorf("chats_in_db = %d, want 2", total)
	}
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1 (both JID forms merge into one human)", len(units))
	}
	u := units[0]
	if u.primary != xvLIDJID {
		t.Errorf("primary = %q, want the most recently active form %q", u.primary, xvLIDJID)
	}
	if len(u.members) != 2 {
		t.Errorf("members = %v, want both JID forms", u.members)
	}
	if u.msgCount != 3 {
		t.Errorf("msgCount = %d, want 3 (summed across forms)", u.msgCount)
	}
	if u.lastMessageTs != xvLIDLastTS {
		t.Errorf("lastMessageTs = %d, want %d", u.lastMessageTs, xvLIDLastTS)
	}
	if len(u.aliasJIDs) != 1 || u.aliasJIDs[0] != xvPhoneJID {
		t.Errorf("aliasJIDs = %v, want [%q]", u.aliasJIDs, xvPhoneJID)
	}
	if u.phone != "15555550100" {
		t.Errorf("phone = %q, want the real phone from the @s.whatsapp.net form", u.phone)
	}
	if u.filename != "Alex Rivera.md" {
		t.Errorf("filename = %q, want %q", u.filename, "Alex Rivera.md")
	}
}

func TestAssignFilenamesDisambiguatesSameDisplayName(t *testing.T) {
	db := xvDB(t)
	// Two DIFFERENT humans with the same push name — pre-fix they silently
	// fought over one file, last writer winning.
	const a = "15555550311@s.whatsapp.net"
	const b = "15555550322@s.whatsapp.net"
	xvChat(t, db, a, "direct", "Same Name", xvPhoneLastTS)
	xvChat(t, db, b, "direct", "Same Name", xvPhoneLastTS-10)
	xvContact(t, db, a, "Same Name", "15555550311")
	xvContact(t, db, b, "Same Name", "15555550322")
	xvMsg(t, db, "A1", a, a, "Same Name", "text", "soy a", "", xvPhoneLastTS, false)
	xvMsg(t, db, "B1", b, b, "Same Name", "text", "soy b", "", xvPhoneLastTS-10, false)
	// Two groups with the same subject collide too.
	xvChat(t, db, "120363000000000011@g.us", "group", "Familia", xvGroupLastTS)
	xvChat(t, db, "120363000000000022@g.us", "group", "Familia", xvGroupLastTS-10)
	xvMsg(t, db, "G1", "120363000000000011@g.us", a, "Same Name", "text", "g uno", "", xvGroupLastTS, false)
	xvMsg(t, db, "G2", "120363000000000022@g.us", b, "Same Name", "text", "g dos", "", xvGroupLastTS-10, false)

	units, _, err := buildExportUnits(db, true, 0)
	if err != nil {
		t.Fatalf("buildExportUnits: %v", err)
	}
	names := map[string]bool{}
	for _, u := range units {
		if names[strings.ToLower(u.filename)] {
			t.Fatalf("filename collision survived planning: %q", u.filename)
		}
		names[strings.ToLower(u.filename)] = true
	}
	// The colliding humans get their phone appended; colliding groups their JID digits.
	for _, want := range []string{
		"Same Name (+15555550311).md",
		"Same Name (+15555550322).md",
		"Familia (group 120363000000000011).md",
		"Familia (group 120363000000000022).md",
	} {
		if !names[strings.ToLower(want)] {
			t.Errorf("expected filename %q, have %v", want, names)
		}
	}
}

// PR #64 review, LOW 2: an alias edge pointing at a JID with NO chats row
// bypassed the non-direct edge guard, landing a group-shaped JID inside a
// person file's alias closure. Reconcile honors alias claims, so that stray
// entry could mask a real future chat at that JID from MISSING detection.
// Non-direct-shaped JIDs (by server suffix) never join a direct closure.
func TestBuildExportUnitsRejectsGroupShapedAliasWithoutChatRow(t *testing.T) {
	db := xvDB(t)
	const rowlessGroupJID = "120363999999999999@g.us"
	xvChat(t, db, xvLIDJID, "direct", "Alex Rivera", xvLIDLastTS)
	xvContact(t, db, xvLIDJID, "Alex Rivera", "")
	xvMsg(t, db, "L1", xvLIDJID, xvLIDJID, "Alex Rivera", "text", "hola", "", xvLIDLastTS, false)
	xvAlias(t, db, xvLIDJID, rowlessGroupJID) // corrupt edge: person <-> rowless group JID

	units, _, err := buildExportUnits(db, false, 0)
	if err != nil {
		t.Fatalf("buildExportUnits: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1", len(units))
	}
	for _, a := range units[0].aliasJIDs {
		if a == rowlessGroupJID {
			t.Fatalf("group-shaped JID %s entered a direct chat's alias closure: %v", rowlessGroupJID, units[0].aliasJIDs)
		}
	}
}

func TestReadChatFileIdentityParsesAliasArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	content := "---\n" +
		"type: whatsapp-chat\n" +
		"contact: \"Alex Rivera\"\n" +
		"jid: \"" + xvLIDJID + "\"\n" +
		"alias_jids: [\"" + xvPhoneJID + "\"]\n" +
		"chat_type: \"direct\"\n" +
		"message_count: 7\n" +
		"last_message_ts: 1784999000\n" +
		"---\n\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	id := readChatFileIdentity(path)
	if !id.exists || id.fileType != "whatsapp-chat" || id.jid != xvLIDJID {
		t.Fatalf("identity = %+v", id)
	}
	if len(id.aliasJIDs) != 1 || id.aliasJIDs[0] != xvPhoneJID {
		t.Errorf("aliasJIDs = %v, want [%q]", id.aliasJIDs, xvPhoneJID)
	}
	if id.lastTs != 1784999000 || id.messageCount != 7 || id.chatType != "direct" {
		t.Errorf("identity fields = %+v", id)
	}
	if got := readChatFileIdentity(filepath.Join(dir, "missing.md")); got.exists {
		t.Error("missing file must not claim existence")
	}
}

func TestHealLegacyFilenamesMovesGroupOffPersonName(t *testing.T) {
	dir := t.TempDir()
	legacy := "Alex Rivera.md"
	content := "---\ntype: whatsapp-chat\njid: \"" + xvGroupJID + "\"\nchat_type: \"group\"\nlast_message_ts: 5\n---\n\nold\n"
	if err := os.WriteFile(filepath.Join(dir, legacy), []byte(content), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	units := []exportUnit{{
		members:  []string{xvGroupJID},
		primary:  xvGroupJID,
		chatType: "group",
		display:  "Alex Rivera",
		filename: "Alex Rivera (group).md",
	}}
	healLegacyFilenames(dir, units)

	if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
		t.Errorf("legacy person-named group file still present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Alex Rivera (group).md")); err != nil {
		t.Errorf("healed file missing: %v", err)
	}
	if !units[0].forceRewrite {
		t.Error("healed unit must be force-rewritten so stale content regenerates")
	}
}
