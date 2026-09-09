package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

func newSendsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db
}

func insertSend(t *testing.T, db *sql.DB, draftID, status, mediaPath string) {
	t.Helper()
	var mp any
	if mediaPath != "" {
		mp = mediaPath
	}
	if _, err := db.Exec(`
		INSERT INTO sends (draft_id, recipient_jid, recipient_display, send_type,
		                   content_text, content_file_path, status, created_at, confirmed_at)
		VALUES (?, '15555550100@s.whatsapp.net', 'Prueba', 'file', 'hola', ?, ?, 100, 101)
	`, draftID, mp, status); err != nil {
		t.Fatalf("insert send %s: %v", draftID, err)
	}
}

func sendState(t *testing.T, db *sql.DB, draftID string) (status string, errMsg sql.NullString) {
	t.Helper()
	if err := db.QueryRow(`SELECT status, error_message FROM sends WHERE draft_id = ?`, draftID).
		Scan(&status, &errMsg); err != nil {
		t.Fatalf("read send %s: %v", draftID, err)
	}
	return status, errMsg
}

// TestReconcileInFlightSends pins the behaviour for a send the bridge was in the
// middle of when it stopped. handleConfirmSend flips the row to 'confirmed'
// BEFORE calling SendMessage, so this state means "delivery already attempted,
// outcome unrecorded" — and nothing else in the codebase ever reads it, which is
// why such a row was previously permanent and invisible.
func TestReconcileInFlightSends(t *testing.T) {
	db := newSendsTestDB(t)

	// A media file the stuck row points at; reconciliation must free it, or the
	// bytes leak for the life of the install.
	dir := t.TempDir()
	media := filepath.Join(dir, "stuck.png")
	if err := os.WriteFile(media, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	insertSend(t, db, "stuck-with-media", "confirmed", media)
	insertSend(t, db, "stuck-no-media", "confirmed", "")
	// Every other state must be left strictly alone.
	insertSend(t, db, "still-a-draft", "draft", "")
	insertSend(t, db, "already-sent", "sent", "")
	insertSend(t, db, "already-failed", "failed", "")
	insertSend(t, db, "expired", "expired", "")

	n, err := ReconcileInFlightSends(t.Context(), db)
	if err != nil {
		t.Fatalf("ReconcileInFlightSends: %v", err)
	}
	if n != 2 {
		t.Fatalf("reconciled %d rows, want 2", n)
	}

	for _, id := range []string{"stuck-with-media", "stuck-no-media"} {
		status, errMsg := sendState(t, db, id)
		if status != "failed" {
			t.Fatalf("%s: status = %q, want failed", id, status)
		}
		// The message has to say delivery is UNKNOWN. Without that, the natural
		// reaction is to resend, which risks duplicating a message that did
		// arrive — the whole reason this does not auto-resend.
		if !errMsg.Valid || errMsg.String != inFlightSendNotice {
			t.Fatalf("%s: error_message = %q, want the in-flight notice", id, errMsg.String)
		}
	}

	if _, err := os.Stat(media); !os.IsNotExist(err) {
		t.Fatalf("the stuck row's media bytes should have been freed, stat err = %v", err)
	}

	for id, want := range map[string]string{
		"still-a-draft":  "draft",
		"already-sent":   "sent",
		"already-failed": "failed",
		"expired":        "expired",
	} {
		if status, _ := sendState(t, db, id); status != want {
			t.Fatalf("%s: status = %q, want %q (untouched)", id, status, want)
		}
	}
}

func TestReconcileInFlightSendsIsIdempotent(t *testing.T) {
	db := newSendsTestDB(t)
	insertSend(t, db, "stuck", "confirmed", "")

	// Runs on every boot; a second pass must find nothing left to do rather
	// than re-reporting the same row forever.
	if n, err := ReconcileInFlightSends(t.Context(), db); err != nil || n != 1 {
		t.Fatalf("first pass: n=%d err=%v, want 1/nil", n, err)
	}
	if n, err := ReconcileInFlightSends(t.Context(), db); err != nil || n != 0 {
		t.Fatalf("second pass: n=%d err=%v, want 0/nil", n, err)
	}
}

func TestReconcileInFlightSendsNoRows(t *testing.T) {
	db := newSendsTestDB(t)
	// The overwhelmingly common case: a clean shutdown left nothing behind, so
	// startup must be silent and cheap.
	if n, err := ReconcileInFlightSends(t.Context(), db); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil", n, err)
	}
}
