package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A control character is legal in a POSIX filename and rejected by Win32, so
// an unstripped one exported green on macOS/Linux and failed the ENTIRE run
// on Windows (every unit dropped → "export incomplete"). Group subjects and
// push names are both attacker-settable.
func TestSanitizeFilenameStripsControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Team\nStandup", "Team-Standup"},
		{"Team\rStandup", "Team-Standup"},
		{"Team\tStandup", "Team-Standup"},
		{"Team\x07Standup", "Team-Standup"},
		{"Team\x00Standup", "Team-Standup"},
		{"Team\x7fStandup", "Team-Standup"},
		{"\n", "-"},                  // degenerate but legal, same as sanitizeFilename("/")
		{"José Muñoz", "José Muñoz"}, // non-ASCII must survive untouched
	}
	for _, c := range cases {
		got := sanitizeFilename(c.in)
		if got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("sanitizeFilename(%q) = %q still carries control char %#U", c.in, got, r)
			}
		}
	}
}

// End-to-end through the public entry point: a chat whose name carries a
// newline must export on every platform, not just the POSIX ones.
func TestExportSurvivesControlCharInChatName(t *testing.T) {
	db := xvDB(t)
	out := t.TempDir()
	jid := "120363000000000099@g.us"
	xvChat(t, db, jid, "group", "Team\nStandup", 1785000000)
	xvMsg(t, db, "m1", jid, "15555550100@s.whatsapp.net", "Alex Rivera",
		"text", "hello", "", 1785000000, false)

	if err := ExportVault(db, out, true, 0); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if name := xvFileForJID(t, out, jid); name == "" {
		ents, _ := os.ReadDir(out)
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		t.Fatalf("chat %s was dropped; dir holds %v", jid, got)
	}
}

// The WhatsApp message ID is chosen by the SENDING client and lands in the
// media filename. filepath.Join cleans rather than confines, so an unguarded
// stem escaped MediaPath — on Windows `\` separates too.
func TestSafeMediaStemCannotEscapeMediaDir(t *testing.T) {
	base := filepath.Join("C:\\", "Users", "victim", ".claude", "whatsapp-mcp", "media")
	hostile := []string{
		`..\..\..\..\Users\victim\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup\pwn`,
		`../../../../Users/victim/.claude/settings`,
		`..\..\evil`,
		`../..`,
		`..`,
		`.`,
		``,
		`a/b`,
		`a:b`,
	}
	for _, id := range hostile {
		stem := safeMediaStem(id)
		if strings.ContainsAny(stem, `/\:`) {
			t.Errorf("safeMediaStem(%q) = %q still holds a separator", id, stem)
		}
		got := filepath.Join(base, stem+".jpg")
		if !strings.HasPrefix(filepath.Clean(got), filepath.Clean(base)+string(filepath.Separator)) {
			t.Errorf("safeMediaStem(%q) escapes: %s", id, got)
		}
	}
	// Benign IDs must pass through untouched so existing files still resolve.
	for _, id := range []string{"3EB0C767D82B1A4F1B2C", "ABC-123_x.9"} {
		if got := safeMediaStem(id); got != id {
			t.Errorf("safeMediaStem(%q) = %q, want unchanged", id, got)
		}
	}
	// Two distinct hostile IDs must not collide onto one file.
	if safeMediaStem(`../../a`) == safeMediaStem(`..\..\a`) {
		t.Errorf("distinct hostile IDs collided")
	}
}
