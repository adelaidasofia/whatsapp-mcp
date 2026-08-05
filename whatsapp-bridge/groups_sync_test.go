package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// SyncGroupMetadata is the source-side fix for MYC-3555's person-named group
// rows: the live-receive path used to stamp the first sender's push name onto
// the group's chats.name and never populated participants_count at all.

func TestSyncGroupMetadataHealsPersonNamedGroupRow(t *testing.T) {
	db := xvDB(t)

	// The bug shape: a group row named after a person, no participant count.
	xvChat(t, db, xvGroupJID, "group", "Alex Rivera", xvGroupLastTS)

	g := &types.GroupInfo{
		JID:              types.JID{User: "120363000000000001", Server: "g.us"},
		ParticipantCount: 12,
	}
	g.Name = "Real Subject"

	n, err := SyncGroupMetadata(context.Background(), db, fakeGroupLister{groups: []*types.GroupInfo{g, nil}})
	if err != nil {
		t.Fatalf("SyncGroupMetadata: %v", err)
	}
	if n != 1 {
		t.Fatalf("groups written = %d, want 1 (nil entries skipped)", n)
	}

	var name string
	var count int
	if err := db.QueryRow(`SELECT COALESCE(name,''), COALESCE(participants_count,0) FROM chats WHERE jid = ?`, xvGroupJID).
		Scan(&name, &count); err != nil {
		t.Fatalf("read chat row: %v", err)
	}
	if name != "Real Subject" {
		t.Errorf("group name = %q, want the real subject (person-named row must heal)", name)
	}
	if count != 12 {
		t.Errorf("participants_count = %d, want 12", count)
	}
}

func TestSyncGroupMetadataInsertsUnknownGroupAndKeepsNameWhenSubjectEmpty(t *testing.T) {
	db := xvDB(t)

	// A group we have no row for yet, whose subject is not in the synced state:
	// the row must still be created (count from the participant list) and a
	// later sync with an empty name must not blank an existing one.
	g := &types.GroupInfo{JID: types.JID{User: "120363000000000002", Server: "g.us"}}
	g.Participants = []types.GroupParticipant{
		{JID: phoneJID("15555550111")},
		{JID: phoneJID("15555550122")},
	}
	if _, err := SyncGroupMetadata(context.Background(), db, fakeGroupLister{groups: []*types.GroupInfo{g}}); err != nil {
		t.Fatalf("SyncGroupMetadata: %v", err)
	}

	var name string
	var count int
	jid := "120363000000000002@g.us"
	if err := db.QueryRow(`SELECT COALESCE(name,''), COALESCE(participants_count,0) FROM chats WHERE jid = ?`, jid).
		Scan(&name, &count); err != nil {
		t.Fatalf("group row was not inserted: %v", err)
	}
	if count != 2 {
		t.Errorf("participants_count = %d, want 2 (fallback to participant list length)", count)
	}

	// Name arrives later, then an empty-subject sync must not erase it.
	g.Name = "Named Later"
	if _, err := SyncGroupMetadata(context.Background(), db, fakeGroupLister{groups: []*types.GroupInfo{g}}); err != nil {
		t.Fatalf("SyncGroupMetadata (named): %v", err)
	}
	g.Name = ""
	if _, err := SyncGroupMetadata(context.Background(), db, fakeGroupLister{groups: []*types.GroupInfo{g}}); err != nil {
		t.Fatalf("SyncGroupMetadata (empty name): %v", err)
	}
	if err := db.QueryRow(`SELECT COALESCE(name,'') FROM chats WHERE jid = ?`, jid).Scan(&name); err != nil {
		t.Fatalf("read chat row: %v", err)
	}
	if name != "Named Later" {
		t.Errorf("name = %q, want %q (empty subject must never blank a known name)", name, "Named Later")
	}
}
