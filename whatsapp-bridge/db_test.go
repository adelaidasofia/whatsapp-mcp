package main

import (
	"database/sql"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Regression coverage for the migration loader.
//
// The bug (v0.2.0, fresh Windows install): applyMigrations read each embedded
// migration with filepath.Join("migrations", name). embed.FS keys are
// forward-slash only on every OS, but filepath.Join uses "\" on Windows, so
// ReadFile failed and the bridge could not migrate its DB on first boot.
//
// The loader had no test at all, which is why the windows-latest leg of
// tests.yml (CGO + sqlcipher) never caught it. Both tests below run on that
// leg. They pass unchanged on macOS/Linux, where filepath.Join and path.Join
// are identical, so the Windows leg is where they earn their keep.

// TestEmbeddedMigrationsResolve reads every embedded migration through the
// same forward-slash key construction the loader uses. If a *.sql file is
// added but not embedded (or renamed), or the embed directive changes, this
// fails on every platform.
func TestEmbeddedMigrationsResolve(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		key := path.Join("migrations", e.Name())
		b, err := migrationsFS.ReadFile(key)
		if err != nil {
			t.Fatalf("embedded migration %q unreadable at key %q: %v", e.Name(), key, err)
		}
		if len(b) == 0 {
			t.Fatalf("embedded migration %q is empty", e.Name())
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no embedded migrations found; embed directive or filenames changed")
	}
}

// TestApplyMigrationsFreshDB runs the real loader end-to-end against a fresh
// unencrypted SQLite DB (sqlcipher driver, no key — matches the EncryptDB=false
// production path). It exercises the exact ReadFile call site the Windows bug
// lived in, so a revert to filepath.Join fails this test on the windows leg.
func TestApplyMigrationsFreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // mirror production: SQLite is single-writer.

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations on fresh db: %v", err)
	}
	// Re-running must be a no-op: 001 is idempotent (IF NOT EXISTS / OR IGNORE)
	// and 002/003 are skipped via schema_version. A second run that errors would
	// mean the non-idempotent ALTER TABLE in 002 ran twice.
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations second run (idempotency): %v", err)
	}
	// Proof the migrations actually executed, not just that files were read:
	// the contacts table from 001 must exist.
	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='contacts'`,
	).Scan(&name); err != nil {
		t.Fatalf("expected 'contacts' table after migrations: %v", err)
	}
}

// --- migration 004: type-CHECK widening + raw_type (MYC-3284) ---------------

// TestMigration004WidensTypeCheck proves 004 widened the constraint rather than
// dropping it. A rebuild that silently lost the CHECK would let any string into
// messages.type and would look identical from the happy path.
func TestMigration004WidensTypeCheck(t *testing.T) {
	db := freshMigratedDB(t)

	if _, err := db.Exec(`INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES ('c@g.us','group',0,0)`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	// The three types 004 adds must be accepted.
	for _, typ := range []string{"poll", "event", "unsupported"} {
		if _, err := db.Exec(
			`INSERT INTO messages (id, chat_jid, timestamp, type, raw_type) VALUES (?,?,?,?,?)`,
			"id-"+typ, "c@g.us", 1, typ, "someRawType",
		); err != nil {
			t.Errorf("type %q rejected but should be allowed after 004: %v", typ, err)
		}
	}

	// A type outside the list must STILL be rejected. Without this, "the
	// migration worked" and "the migration destroyed the constraint" look the same.
	if _, err := db.Exec(
		`INSERT INTO messages (id, chat_jid, timestamp, type) VALUES ('bad','c@g.us',1,'not_a_real_type')`,
	); err == nil {
		t.Fatal("CHECK constraint accepted an invalid type: 004 dropped the constraint instead of widening it")
	}
}

// TestMigration004PreservesLegacyColumnsAndIndexes guards the table rebuild.
// The INSERT..SELECT lists every column by name; a column added in 001/002 and
// forgotten there would be silently dropped for every existing install.
func TestMigration004PreservesLegacyColumnsAndIndexes(t *testing.T) {
	db := freshMigratedDB(t)

	wantCols := []string{
		"id", "chat_jid", "sender_jid", "sender_display", "timestamp", "type",
		"content_text", "content_normalized", "media_path", "media_mime",
		"media_duration_sec", "quoted_message_id", "reactions_json",
		"is_from_me", "is_edited", "is_deleted",
		"voice_note_transcript", "voice_note_transcript_backend", "voice_note_transcript_at",
		"scrubbed_text", "scrub_flags_json",
		"media_key", "media_direct_path", "media_url", "media_enc_sha256",
		"media_sha256", "media_file_length", "media_key_timestamp",
		"raw_type",
	}
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('messages')`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		have[n] = true
	}
	for _, c := range wantCols {
		if !have[c] {
			t.Errorf("column %q missing after the 004 table rebuild", c)
		}
	}

	// Every index from 001 and 002 must survive the rebuild, plus 004's own.
	wantIdx := []string{
		"idx_messages_chat_time", "idx_messages_sender_time", "idx_messages_type",
		"idx_messages_content_normalized", "idx_messages_quoted",
		"idx_messages_voice_pending", "idx_messages_undecoded",
	}
	for _, idx := range wantIdx {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&name); err != nil {
			t.Errorf("index %q missing after the 004 table rebuild: %v", idx, err)
		}
	}
}

// TestMigration004IsAtomic asserts 004 is wrapped in a transaction. The runner
// executes each file as one db.Exec with no surrounding transaction, so a
// failure between DROP TABLE messages and the RENAME would destroy the whole
// message history unrecoverably.
func TestMigration004IsAtomic(t *testing.T) {
	b, err := migrationsFS.ReadFile(path.Join("migrations", "004_message_type_widen.sql"))
	if err != nil {
		t.Fatalf("read 004: %v", err)
	}
	sqlText := strings.ToUpper(string(b))
	if !strings.Contains(sqlText, "BEGIN;") || !strings.Contains(sqlText, "COMMIT;") {
		t.Fatal("004 drops and recreates the messages table but is not wrapped in BEGIN/COMMIT: " +
			"a mid-migration failure would leave the store with no messages table and no rollback")
	}
}

func freshMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db
}
