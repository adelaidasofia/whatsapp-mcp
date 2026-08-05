package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// groupLister is the slice of the whatsmeow client that the groups endpoint
// needs. *whatsmeow.Client satisfies it; tests inject a fake so the mapping
// logic is covered without a live WhatsApp session.
type groupLister interface {
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
}

type groupParticipant struct {
	JID         string `json:"jid"`
	Phone       string `json:"phone,omitempty"` // E.164 digits, when the number is known
	IsAdmin     bool   `json:"is_admin,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type groupSummary struct {
	JID              string             `json:"jid"`
	Name             string             `json:"name"`
	ParticipantCount int                `json:"participant_count"`
	Participants     []groupParticipant `json:"participants"`
	Created          string             `json:"created,omitempty"` // RFC3339 UTC
}

type groupListResponse struct {
	Groups []groupSummary `json:"groups"`
	Total  int            `json:"total"`
}

// buildGroupList maps whatsmeow's joined-group state into the API response.
// Phone numbers come from each participant's resolved PhoneNumber JID; a
// participant whose number is privacy-hidden (LID-only) yields no phone rather
// than leaking the opaque LID as if it were a phone number.
func buildGroupList(ctx context.Context, lister groupLister) (groupListResponse, error) {
	groups, err := lister.GetJoinedGroups(ctx)
	if err != nil {
		return groupListResponse{}, err
	}
	resp := groupListResponse{Groups: []groupSummary{}}
	for _, g := range groups {
		if g == nil {
			continue
		}
		gs := groupSummary{
			JID:              g.JID.String(),
			Name:             g.Name,
			ParticipantCount: g.ParticipantCount,
			Participants:     []groupParticipant{},
		}
		if gs.ParticipantCount == 0 {
			gs.ParticipantCount = len(g.Participants)
		}
		if !g.GroupCreated.IsZero() {
			gs.Created = g.GroupCreated.UTC().Format(time.RFC3339)
		}
		for _, p := range g.Participants {
			part := groupParticipant{
				JID:         p.JID.String(),
				IsAdmin:     p.IsAdmin || p.IsSuperAdmin,
				DisplayName: p.DisplayName,
			}
			if p.PhoneNumber.User != "" {
				part.Phone = p.PhoneNumber.User
			}
			gs.Participants = append(gs.Participants, part)
		}
		resp.Groups = append(resp.Groups, gs)
	}
	resp.Total = len(resp.Groups)
	return resp, nil
}

// UpsertGroupChat writes a group's AUTHORITATIVE metadata — its real subject
// and participant count — into the chats table.
//
// This is the source-side fix for MYC-3555's person-named group files: the
// live-receive path cannot know the group subject (events.Message does not
// carry it), so before this, a group born from a live message was permanently
// named after whichever member sent the first message the bridge saw, and its
// participants_count was never populated at all (the vault export then emitted
// participants_count: 0, which slid under every group-size policy).
func UpsertGroupChat(db *sql.DB, g *types.GroupInfo) error {
	if g == nil {
		return nil
	}
	count := g.ParticipantCount
	if count == 0 {
		count = len(g.Participants)
	}
	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO chats (jid, chat_type, name, normalized_name, participants_count, created_at, updated_at)
		VALUES (?, 'group', ?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			normalized_name = CASE WHEN excluded.name <> '' THEN excluded.normalized_name ELSE chats.normalized_name END,
			participants_count = excluded.participants_count,
			updated_at = excluded.updated_at
	`, g.JID.String(), g.Name, Normalize(g.Name), count, now, now)
	if err != nil {
		return fmt.Errorf("upsert group chat %s: %w", g.JID.String(), err)
	}
	return nil
}

// SyncGroupMetadata pulls every joined group from whatsmeow's synced state and
// upserts its subject + participant count into chats. Runs at startup (async,
// after the connection settles) so historical person-named group rows heal and
// participants_count becomes real data rather than a NULL the export has to
// floor-estimate. Idempotent. Returns how many groups were written.
func SyncGroupMetadata(ctx context.Context, db *sql.DB, lister groupLister) (int, error) {
	if lister == nil {
		return 0, fmt.Errorf("SyncGroupMetadata: no group lister available")
	}
	groups, err := lister.GetJoinedGroups(ctx)
	if err != nil {
		return 0, fmt.Errorf("SyncGroupMetadata: %w", err)
	}
	written := 0
	for _, g := range groups {
		if g == nil {
			continue
		}
		if err := UpsertGroupChat(db, g); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// handleListGroups serves GET /api/groups: every joined group with its
// participants and their phone numbers, taken from whatsmeow's synced group
// state (not the chats table, which only knows groups with recent messages).
//
// Loopback-only, like every other endpoint on this server (enforced in
// config.Validate). It exposes the same contact-PII class already served by
// GET /api/contacts/search, so it adds no new exposure surface.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	connected, authed, _, _ := s.bridge.Status()
	if !authed {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error:   "not paired",
			Details: "device is not paired; complete pairing before listing groups",
		})
		return
	}
	if !connected || s.bridge.client == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error:   "not connected",
			Details: "bridge is offline; joined groups come from the live synced session",
		})
		return
	}
	resp, err := buildGroupList(r.Context(), s.bridge.client)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error:   "list groups failed",
			Details: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
