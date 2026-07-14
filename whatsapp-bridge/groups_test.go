package main

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

type fakeGroupLister struct {
	groups []*types.GroupInfo
	err    error
}

func (f fakeGroupLister) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	return f.groups, f.err
}

func phoneJID(user string) types.JID { return types.JID{User: user, Server: "s.whatsapp.net"} }

// A phone participant's number is surfaced; an LID-only participant (number
// privacy-hidden) must NOT leak its opaque LID as a phone. A nil group entry
// is skipped. ParticipantCount and name map through.
func TestBuildGroupList_MapsNamePhonesAndProtectsLID(t *testing.T) {
	g := &types.GroupInfo{
		JID:              types.JID{User: "123456", Server: "g.us"},
		ParticipantCount: 2,
	}
	g.Name = "Client A"
	g.Participants = []types.GroupParticipant{
		{JID: phoneJID("15551230000"), PhoneNumber: phoneJID("15551230000"), IsAdmin: true},
		{JID: types.JID{User: "998877665544", Server: "lid"}}, // LID-only, no PhoneNumber
	}

	resp, err := buildGroupList(context.Background(), fakeGroupLister{groups: []*types.GroupInfo{g, nil}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 1 || len(resp.Groups) != 1 {
		t.Fatalf("want 1 group, got total=%d len=%d", resp.Total, len(resp.Groups))
	}
	grp := resp.Groups[0]
	if grp.Name != "Client A" {
		t.Errorf("name = %q, want %q", grp.Name, "Client A")
	}
	if grp.ParticipantCount != 2 {
		t.Errorf("participant_count = %d, want 2", grp.ParticipantCount)
	}
	if len(grp.Participants) != 2 {
		t.Fatalf("participants len = %d, want 2", len(grp.Participants))
	}
	if grp.Participants[0].Phone != "15551230000" {
		t.Errorf("participant[0].phone = %q, want 15551230000", grp.Participants[0].Phone)
	}
	if !grp.Participants[0].IsAdmin {
		t.Errorf("participant[0] should be admin")
	}
	if grp.Participants[1].Phone != "" {
		t.Errorf("LID-only participant leaked a phone: %q", grp.Participants[1].Phone)
	}
}

func TestBuildGroupList_PropagatesError(t *testing.T) {
	if _, err := buildGroupList(context.Background(), fakeGroupLister{err: errors.New("boom")}); err == nil {
		t.Fatal("expected the lister error to propagate")
	}
}

func TestBuildGroupList_CountFallsBackToParticipantLen(t *testing.T) {
	g := &types.GroupInfo{JID: types.JID{User: "1", Server: "g.us"}}
	g.Participants = []types.GroupParticipant{{JID: phoneJID("2")}}
	resp, err := buildGroupList(context.Background(), fakeGroupLister{groups: []*types.GroupInfo{g}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Groups[0].ParticipantCount != 1 {
		t.Errorf("count fallback = %d, want 1", resp.Groups[0].ParticipantCount)
	}
}

func TestBuildGroupList_EmptyIsNonNilSlice(t *testing.T) {
	resp, err := buildGroupList(context.Background(), fakeGroupLister{groups: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Groups == nil {
		t.Error("Groups should marshal as [] not null")
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}
