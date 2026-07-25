package main

import (
	"database/sql"
	"testing"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// Follow-up hardening to PR #40. The exclude-chats filter is a PRIVACY control:
// a member listing a chat in WHATSAPP_WHISPER_EXCLUDE_CHATS is saying "never
// transcribe this". If the chat lookup errors we cannot tell whether this chat
// is on that list, and the two failure directions are not symmetric — skipping
// a transcript is recoverable, transcribing a private voice note is not. So the
// filter must fail CLOSED.
func TestChatExcluded_FailsClosedOnLookupError(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:exclude_failclosed?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Closing the handle makes every subsequent query return an error, which is
	// the condition under test (a busy/locked/corrupt store behaves the same).
	_ = db.Close()

	tr := &Transcriber{cfg: &Config{WhisperExcludeChats: []string{"mama"}}, db: db}
	if !tr.chatExcluded("999@s.whatsapp.net") {
		t.Fatal("privacy filter failed OPEN on a lookup error: it returned false, so a chat the member excluded would be transcribed")
	}
}

// The fail-closed rule must not swallow the ordinary case: a chat with no row
// in `chats` yet is NOT an error, and the JID itself is still matched.
func TestChatExcluded_MissingRowIsNotAnError(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:exclude_norow?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, normalized_name TEXT)`); err != nil {
		t.Fatalf("create chats: %v", err)
	}

	// No row for this JID. The pattern matches the JID, so it is excluded.
	tr := &Transcriber{cfg: &Config{WhisperExcludeChats: []string{"999"}}, db: db}
	if !tr.chatExcluded("999@s.whatsapp.net") {
		t.Fatal("a JID-matching pattern must still exclude when the chats row is absent")
	}

	// No row and no pattern match: allowed through, NOT swept up by fail-closed.
	tr2 := &Transcriber{cfg: &Config{WhisperExcludeChats: []string{"zzz-no-match"}}, db: db}
	if tr2.chatExcluded("999@s.whatsapp.net") {
		t.Fatal("a missing chats row must not be treated as a lookup error; this chat should transcribe normally")
	}
}
