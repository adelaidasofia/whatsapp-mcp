package main

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MYC-3284 items 4 + 5 — an undecodable message must be VISIBLE in the vault
// export and MEASURABLE on /healthcheck. Both are DB-backed (real schema via
// applyMigrations, real queries) rather than asserted against a mock.

func undecodedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
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

func insertTestMessage(t *testing.T, db *sql.DB, id, chatJID, msgType, contentText string, ts int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, is_from_me)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		id, chatJID, "31628239888478:3@lid", "Martha", ts, msgType, contentText)
	if err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

// The marker is the contract between the writer (extractContent) and every
// reader (export, healthcheck). A round-trip test is what stops the two halves
// from drifting apart silently.
func TestUnsupportedMarkerRoundTrip(t *testing.T) {
	if got := unsupportedRawType(unsupportedMarker("eventMessage")); got != "eventMessage" {
		t.Fatalf("round-trip: want \"eventMessage\", got %q", got)
	}
	for _, notAMarker := range []string{"", "hello", "[unsupported: unterminated", "unsupported: x]"} {
		if got := unsupportedRawType(notAMarker); got != "" {
			t.Fatalf("non-marker %q must yield \"\", got %q", notAMarker, got)
		}
	}
}

// Item 5 — the export renders an explicit placeholder. The negative control is
// the `if text == "" { continue }` path: an undecodable message must never be
// silently OMITTED from the chat file.
func TestExportRendersUnsupportedPlaceholder(t *testing.T) {
	db := undecodedTestDB(t)
	const jid = "12147735814-1589465137@g.us"
	insertTestMessage(t, db, "A1", jid, "text", "hello from the group", 1784846600)
	// Stored the way the bridge stores it: an allowed type, with the marker
	// carrying the signal (see extractContent — messages.type is CHECK-constrained).
	insertTestMessage(t, db, "A2", jid, "system", unsupportedMarker("eventMessage"), 1784846683)

	outDir := t.TempDir()
	if _, err := exportOneChat(db, outDir, jid, "group", "Family", 3, 1784846683, "2026-07-24"); err != nil {
		t.Fatalf("exportOneChat: %v", err)
	}

	var body string
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		body += string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if body == "" {
		t.Fatal("export wrote no markdown file")
	}
	if !strings.Contains(body, "hello from the group") {
		t.Fatalf("regressed the normal text path; export was:\n%s", body)
	}
	if !strings.Contains(body, "[Unsupported message: eventMessage]") {
		t.Fatalf("MYC-3284 regression: undecodable message missing or unlabelled in the export:\n%s", body)
	}
	// It must carry the sender, like [Voice note] does — not an orphan line.
	if !strings.Contains(body, "Martha: [Unsupported message: eventMessage]") {
		t.Fatalf("placeholder must be attributed to its sender; export was:\n%s", body)
	}
}

// Item 4 — /healthcheck can count what the bridge could not read, including the
// PRE-floor rows still sitting in the store (the size of the backfill).
func TestUndecodedStatsCountsCaughtAndLegacyRows(t *testing.T) {
	db := undecodedTestDB(t)
	const jid = "12147735814-1589465137@g.us"
	insertTestMessage(t, db, "B1", jid, "text", "a normal message", 1784846600)
	insertTestMessage(t, db, "B2", jid, "system", unsupportedMarker("eventMessage"), 1784846601)
	insertTestMessage(t, db, "B3", jid, "system", unsupportedMarker("eventMessage"), 1784846602)
	insertTestMessage(t, db, "B4", jid, "system", unsupportedMarker("pollUpdateMessage"), 1784846603)
	// A malformed marker still counts — reported as "unknown", never dropped.
	insertTestMessage(t, db, "B5", jid, "system", unsupportedPrefix+"truncated", 1784846604)
	// Pre-floor silent drops: empty "system" rows, the exact backfill target.
	insertTestMessage(t, db, "B6", jid, "system", "", 1784846605)
	insertTestMessage(t, db, "B7", jid, "system", "", 1784846606)
	// A system row that DOES carry text is not a silent drop.
	insertTestMessage(t, db, "B8", jid, "system", "group name changed", 1784846607)

	s := &Server{db: db}
	st := s.undecodedStats(context.Background())

	if st.UndecodedTotal != 4 {
		t.Fatalf("undecoded_total: want 4, got %d", st.UndecodedTotal)
	}
	if st.UndecodedByType["eventMessage"] != 2 {
		t.Fatalf("undecoded_by_type[eventMessage]: want 2, got %d (%v)", st.UndecodedByType["eventMessage"], st.UndecodedByType)
	}
	if st.UndecodedByType["pollUpdateMessage"] != 1 {
		t.Fatalf("undecoded_by_type[pollUpdateMessage]: want 1, got %d (%v)", st.UndecodedByType["pollUpdateMessage"], st.UndecodedByType)
	}
	if st.UndecodedByType["unknown"] != 1 {
		t.Fatalf("an unsupported row with no marker must count as \"unknown\", got %v", st.UndecodedByType)
	}
	if st.LegacyEmptySystem != 2 {
		t.Fatalf("legacy_empty_system: want 2 (the backfill size), got %d", st.LegacyEmptySystem)
	}
}
