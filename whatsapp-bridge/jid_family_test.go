package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestJIDFamilyIsOnePersonUnderEveryJID(t *testing.T) {
	db := newSendsTestDB(t)
	if _, err := db.Exec(`INSERT INTO jid_aliases (jid_a, jid_b, discovered_at, source) VALUES (?, ?, 1, 'test')`,
		"15555550100@s.whatsapp.net", "77777777777777@lid"); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	family := s.jidFamily(t.Context(), "15555550100:3@s.whatsapp.net")
	if !slices.Equal(family, []string{"15555550100@s.whatsapp.net", "77777777777777@lid"}) {
		t.Fatalf("family = %v: the device part is the same chat, and the LID is the same person", family)
	}
	if got := s.jidFamily(t.Context(), "120363000000000001@g.us"); !slices.Equal(got, []string{"120363000000000001@g.us"}) {
		t.Fatalf("a group with no aliases is its own family, got %v", got)
	}
}

func TestAliasesEndpointAndChatRowsCarryTheFamily(t *testing.T) {
	db := newSendsTestDB(t)
	if _, err := db.Exec(`INSERT INTO jid_aliases (jid_a, jid_b, discovered_at, source) VALUES (?, ?, 1, 'test')`,
		"77777777777777@lid", "15555550100@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chats (jid, chat_type, name, last_message_time, last_message_preview, created_at, updated_at) VALUES ('77777777777777@lid', 'direct', 'Lucía', 100, 'hola', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, bridge: &Bridge{}}

	rec := httptest.NewRecorder()
	s.handleAliases(rec, httptest.NewRequest(http.MethodGet, "/api/aliases?jid=77777777777777@lid", nil))
	var aliases struct{ Family []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &aliases); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(aliases.Family, "15555550100@s.whatsapp.net") {
		t.Fatalf("/api/aliases family = %v", aliases.Family)
	}

	rec = httptest.NewRecorder()
	s.handleListChats(rec, httptest.NewRequest(http.MethodGet, "/api/chats?limit=20", nil))
	var chats chatListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &chats); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(chats.Chats) != 1 || !slices.Equal(chats.Chats[0].Aliases, []string{"15555550100@s.whatsapp.net"}) {
		t.Fatalf("chat rows = %+v, want the LID row to carry its phone JID", chats.Chats)
	}
}
