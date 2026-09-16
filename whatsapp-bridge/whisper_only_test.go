package main

import (
	"database/sql"
	"testing"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// WHATSAPP_WHISPER_ONLY_CHATS is the inverse of the exclude list: a member who
// wants transcription on a single chat (typically "message yourself") should
// not have to enumerate every other contact, and every contact added later, in
// the exclude list. These tests pin that the allow-list is a privacy control
// with the same fail-closed posture as the exclude list.

func onlyChatsDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, normalized_name TEXT)`); err != nil {
		t.Fatalf("create chats: %v", err)
	}
	rows := [][3]string{
		{"111@lid", "Me, Myself", "me, myself"},
		{"222@s.whatsapp.net", "Mamá", "mama"},
		{"111-1700000000@g.us", "Group created by me", "group created by me"},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO chats (jid, name, normalized_name) VALUES (?, ?, ?)`, r[0], r[1], r[2]); err != nil {
			t.Fatalf("insert chat: %v", err)
		}
	}
	return db
}

func TestChatExcluded_OnlyChatsAllowsMatchAndExcludesTheRest(t *testing.T) {
	db := onlyChatsDB(t, "only_basic")
	tr := &Transcriber{cfg: &Config{WhisperOnlyChats: []string{"111@lid"}}, db: db}

	if tr.chatExcluded("111@lid") {
		t.Fatal("the only allowed chat was excluded")
	}
	if !tr.chatExcluded("222@s.whatsapp.net") {
		t.Fatal("a chat outside the only-chats list was transcribed")
	}
	// A full-JID pattern must not leak into a group whose JID starts with the
	// same number (groups created by the member embed their number).
	if !tr.chatExcluded("111-1700000000@g.us") {
		t.Fatal("a group sharing the member's number was let through by a full-JID pattern")
	}
	// A chat with no row yet is still matched on its JID, not waved through.
	if !tr.chatExcluded("333@s.whatsapp.net") {
		t.Fatal("an unknown chat outside the only-chats list was transcribed")
	}
}

func TestChatExcluded_OnlyChatsMatchesByName(t *testing.T) {
	db := onlyChatsDB(t, "only_name")
	tr := &Transcriber{cfg: &Config{WhisperOnlyChats: []string{"myself"}}, db: db}
	if tr.chatExcluded("111@lid") {
		t.Fatal("an only-chats pattern matching the chat name did not allow it")
	}
}

func TestChatExcluded_ExcludeWinsOverOnly(t *testing.T) {
	db := onlyChatsDB(t, "only_exclude_wins")
	tr := &Transcriber{cfg: &Config{
		WhisperOnlyChats:    []string{"111@lid"},
		WhisperExcludeChats: []string{"myself"},
	}, db: db}
	if !tr.chatExcluded("111@lid") {
		t.Fatal("a chat on both lists was transcribed; exclude must win")
	}
}

func TestChatExcluded_OnlyChatsFailsClosed(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:only_failclosed?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_ = db.Close()
	tr := &Transcriber{cfg: &Config{WhisperOnlyChats: []string{"111@lid"}}, db: db}
	if !tr.chatExcluded("111@lid") {
		t.Fatal("allow-list failed OPEN on a lookup error")
	}
	if !tr.chatExcluded("") {
		t.Fatal("allow-list let an unidentified chat (empty JID) through")
	}
}

func TestChatExcluded_NoListsKeepsOldBehavior(t *testing.T) {
	db := onlyChatsDB(t, "only_none")
	tr := &Transcriber{cfg: &Config{}, db: db}
	if tr.chatExcluded("222@s.whatsapp.net") || tr.chatExcluded("") {
		t.Fatal("with neither list set, nothing may be excluded")
	}
}
