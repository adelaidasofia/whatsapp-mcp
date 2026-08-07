package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// TestSweepOnceNoDeadlockWithExcludeFilter is the regression guard for the
// read-wedge bug: SweepOnce used to call chatExcluded (which runs its own
// chats query) while the pending-rows result set was still open. With
// SetMaxOpenConns(1) — the production setting — the nested query waited
// forever for the connection its own caller held, permanently deadlocking
// the only DB connection; every later read hung until process restart.
// The fix buffers and closes the result set before filtering/enqueueing.
func TestSweepOnceNoDeadlockWithExcludeFilter(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:sweep_deadlock_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // must match production OpenDB or the test proves nothing

	_, err = db.Exec(`
		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			chat_jid TEXT NOT NULL,
			type TEXT NOT NULL,
			timestamp INTEGER NOT NULL,
			voice_note_transcript TEXT,
			media_key BLOB,
			media_direct_path TEXT,
			media_url TEXT,
			media_enc_sha256 BLOB,
			media_sha256 BLOB,
			media_file_length INTEGER,
			media_key_timestamp INTEGER
		);
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, normalized_name TEXT);
		INSERT INTO chats VALUES ('123@s.whatsapp.net', 'Mamá', 'mama');
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	now := time.Now().Unix()
	for _, row := range []struct{ id, jid string }{
		{"MSG1", "123@s.whatsapp.net"}, // excluded by name match
		{"MSG2", "456@s.whatsapp.net"}, // not excluded
	} {
		if _, err := db.Exec(`
			INSERT INTO messages (id, chat_jid, type, timestamp, media_key)
			VALUES (?, ?, 'voice', ?, x'ab')
		`, row.id, row.jid, now); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	cfg := &Config{WhisperBackend: "off", WhisperExcludeChats: []string{"mama"}}
	transcriber := NewTranscriber(cfg, db)
	t.Cleanup(transcriber.Close)
	b := NewTranscriptBackfiller(db, nil, transcriber, 900, 14)

	done := make(chan error, 1)
	go func() {
		_, err := b.SweepOnce(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SweepOnce: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SweepOnce deadlocked: nested query while pending-rows result set held the single pooled connection")
	}

	// The connection must be free again: a follow-up query cannot hang.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("post-sweep query: n=%d err=%v", n, err)
	}
}
