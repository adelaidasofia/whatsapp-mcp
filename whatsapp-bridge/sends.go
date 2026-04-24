package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Send flow: two-step draft then confirm.
//
// 1. POST /api/sends creates a draft row in the `sends` table with status='draft'.
//    Returns draft_id plus a resolved recipient display (so Claude and the user can
//    verify WHO they are about to message before committing).
// 2. POST /api/sends/{draft_id}/confirm flips status to 'confirmed', calls
//    whatsmeow.SendMessage, flips to 'sent' on success. The confirm is the only
//    place an actual outbound network send happens.
//
// Drafts expire after 1 hour so stale drafts can't be confirmed from a forgotten state.

const draftTTLSeconds = 3600

// --- Draft (pre-send) -------------------------------------------------------

type createDraftRequest struct {
	SendType        string `json:"send_type"` // "text" | "reply_quote" | "reaction"
	RecipientJID    string `json:"recipient_jid"`
	Text            string `json:"text,omitempty"`
	QuotedMessageID string `json:"quoted_message_id,omitempty"`
	ReactionEmoji   string `json:"reaction_emoji,omitempty"`   // for reaction: the emoji (e.g., "❤️")
	ReactionTarget  string `json:"reaction_target,omitempty"`  // for reaction: the message ID being reacted to
}

type createDraftResponse struct {
	DraftID             string `json:"draft_id"`
	RecipientJID        string `json:"recipient_jid"`
	RecipientDisplay    string `json:"recipient_display"`
	Preview             string `json:"preview"`
	ExpiresAt           int64  `json:"expires_at"`
	RecipientContactHit bool   `json:"recipient_contact_hit"`
}

func (s *Server) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	var req createDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body", Details: err.Error()})
		return
	}
	if req.RecipientJID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "recipient_jid required"})
		return
	}
	if req.SendType == "" {
		req.SendType = "text"
	}
	switch req.SendType {
	case "text":
		if req.Text == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "text required for send_type=text"})
			return
		}
	case "reply_quote":
		if req.Text == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "text required for send_type=reply_quote"})
			return
		}
		if req.QuotedMessageID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "quoted_message_id required for send_type=reply_quote"})
			return
		}
	case "reaction":
		if req.ReactionEmoji == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "reaction_emoji required for send_type=reaction"})
			return
		}
		if req.ReactionTarget == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "reaction_target required for send_type=reaction (message ID to react to)"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error:   fmt.Sprintf("send_type=%q not supported in v0.6.0", req.SendType),
			Details: "supported: 'text', 'reply_quote', 'reaction'. Media/audio land in v0.7.0.",
		})
		return
	}

	// Resolve recipient display name so Claude/user can verify before confirming.
	recipientDisplay, hit := s.resolveRecipient(r.Context(), req.RecipientJID)

	draftID := uuid.New().String()
	now := time.Now().Unix()

	// For reactions, use the reaction target as the "quoted" message and the emoji as text.
	// That way all drafts fit the same row schema.
	contentText := req.Text
	quotedID := req.QuotedMessageID
	if req.SendType == "reaction" {
		contentText = req.ReactionEmoji
		quotedID = req.ReactionTarget
	}

	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO sends (draft_id, recipient_jid, recipient_display, send_type, content_text, quoted_message_id, reaction_emoji, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'draft', ?)
	`, draftID, req.RecipientJID, recipientDisplay, req.SendType, contentText, nullString(quotedID), nullString(req.ReactionEmoji), now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "draft insert failed", Details: err.Error()})
		return
	}

	preview := truncate(req.Text, 200)
	if req.SendType == "reaction" {
		preview = fmt.Sprintf("Reaction %s to message %s", req.ReactionEmoji, truncate(req.ReactionTarget, 32))
	}

	writeJSON(w, http.StatusOK, createDraftResponse{
		DraftID:             draftID,
		RecipientJID:        req.RecipientJID,
		RecipientDisplay:    recipientDisplay,
		Preview:             preview,
		ExpiresAt:           now + draftTTLSeconds,
		RecipientContactHit: hit,
	})
}

// --- Confirm (commits the send) ---------------------------------------------

type confirmResponse struct {
	DraftID            string `json:"draft_id"`
	Status             string `json:"status"`
	WhatsAppMessageID  string `json:"whatsapp_message_id,omitempty"`
	SentAt             int64  `json:"sent_at,omitempty"`
	RecipientJID       string `json:"recipient_jid"`
	RecipientDisplay   string `json:"recipient_display"`
	Error              string `json:"error,omitempty"`
}

func (s *Server) handleConfirmSend(w http.ResponseWriter, r *http.Request) {
	draftID := r.PathValue("draft_id")
	if draftID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "draft_id required"})
		return
	}
	if s.bridge == nil || !s.bridge.IsConnected() {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error:   "bridge not connected to WhatsApp",
			Details: "cannot send while disconnected; wait for reconnect and retry",
		})
		return
	}

	// Load the draft, validate status and TTL.
	var (
		recipientJID, recipientDisplay                string
		sendType, contentText, quotedID, reactionEmoji string
		status                                        string
		createdAt                                     int64
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT recipient_jid, COALESCE(recipient_display, ''), send_type, COALESCE(content_text, ''),
		        COALESCE(quoted_message_id, ''), COALESCE(reaction_emoji, ''), status, created_at
		 FROM sends WHERE draft_id = ?`, draftID,
	).Scan(&recipientJID, &recipientDisplay, &sendType, &contentText, &quotedID, &reactionEmoji, &status, &createdAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "draft not found", Details: draftID})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "draft lookup failed", Details: err.Error()})
		return
	}
	if status != "draft" {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error:   fmt.Sprintf("draft already in state %q, expected 'draft'", status),
			Details: "each draft can only be confirmed once; create a new draft to retry",
		})
		return
	}
	if time.Now().Unix() > createdAt+draftTTLSeconds {
		// Mark expired.
		_, _ = s.db.ExecContext(r.Context(), `UPDATE sends SET status='expired' WHERE draft_id=?`, draftID)
		writeJSON(w, http.StatusGone, errorResponse{
			Error:   "draft expired",
			Details: fmt.Sprintf("drafts expire after %d seconds; create a new draft", draftTTLSeconds),
		})
		return
	}

	// Parse JID.
	recipient, err := types.ParseJID(recipientJID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid recipient JID", Details: err.Error()})
		return
	}

	// Build the outbound message based on send type.
	var msg *waE2E.Message
	switch sendType {
	case "text":
		msg = &waE2E.Message{
			Conversation: proto.String(contentText),
		}
	case "reply_quote":
		// Look up the quoted message to populate ContextInfo.Participant correctly.
		var quotedSender string
		var quotedFromMe int
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(sender_jid, ''), is_from_me FROM messages WHERE id = ?`, quotedID,
		).Scan(&quotedSender, &quotedFromMe)
		if quotedFromMe == 1 && s.bridge.client.Store.ID != nil {
			quotedSender = s.bridge.client.Store.ID.String()
		}
		msg = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(contentText),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:    proto.String(quotedID),
					Participant: proto.String(quotedSender),
				},
			},
		}
	case "reaction":
		var targetFromMe int
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT is_from_me FROM messages WHERE id = ?`, quotedID,
		).Scan(&targetFromMe)
		msg = &waE2E.Message{
			ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(recipientJID),
					FromMe:    proto.Bool(targetFromMe == 1),
					ID:        proto.String(quotedID),
				},
				Text:              proto.String(reactionEmoji),
				SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
			},
		}
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error:   fmt.Sprintf("unsupported send_type %q reached confirm", sendType),
			Details: "this is a bug; the draft insert validator should have rejected it",
		})
		return
	}

	// Flip to confirmed BEFORE sending, so double-confirm races are blocked.
	now := time.Now().Unix()
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE sends SET status='confirmed', confirmed_at=? WHERE draft_id=? AND status='draft'`,
		now, draftID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "confirm update failed", Details: err.Error()})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "race: draft changed state during confirm"})
		return
	}

	// Execute the send. This is the only place an actual outbound network call happens.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	sendResp, sendErr := s.bridge.client.SendMessage(ctx, recipient, msg)
	if sendErr != nil {
		log.Printf("send failed draft=%s recipient=%s: %v", draftID, recipientJID, sendErr)
		_, _ = s.db.ExecContext(r.Context(),
			`UPDATE sends SET status='failed', error_message=? WHERE draft_id=?`,
			sendErr.Error(), draftID)
		writeJSON(w, http.StatusBadGateway, errorResponse{
			Error:   "send failed",
			Details: sendErr.Error(),
		})
		return
	}

	sentAt := time.Now().Unix()
	whatsappID := sendResp.ID
	_, _ = s.db.ExecContext(r.Context(),
		`UPDATE sends SET status='sent', sent_at=?, whatsapp_message_id=? WHERE draft_id=?`,
		sentAt, whatsappID, draftID)

	// Also persist the sent message in messages table so it shows up in chat history.
	senderJID := ""
	if s.bridge.client.Store.ID != nil {
		senderJID = s.bridge.client.Store.ID.String()
	}
	scrubbed, flags := Scrub(contentText)
	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO messages (id, chat_jid, sender_jid, timestamp, type, content_text, content_normalized, is_from_me, scrubbed_text, scrub_flags_json)
		VALUES (?, ?, ?, ?, 'text', ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, whatsappID, recipientJID, senderJID, sentAt, contentText, Normalize(contentText), scrubbed, ScrubFlagsJSON(flags))
	if err != nil {
		log.Printf("sent-message persist failed: %v", err)
	}

	writeJSON(w, http.StatusOK, confirmResponse{
		DraftID:           draftID,
		Status:            "sent",
		WhatsAppMessageID: whatsappID,
		SentAt:            sentAt,
		RecipientJID:      recipientJID,
		RecipientDisplay:  recipientDisplay,
	})
}

// --- Recipient resolution ---------------------------------------------------

// resolveRecipient looks up a human-readable display for a JID from our contacts table.
// Returns (display, true) if we found a contact; (jid, false) otherwise.
func (s *Server) resolveRecipient(ctx context.Context, jid string) (string, bool) {
	var pushName, verifiedName, phone sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT push_name, verified_name, phone FROM contacts WHERE jid = ?
	`, jid).Scan(&pushName, &verifiedName, &phone)
	if err != nil {
		return jid, false
	}
	if verifiedName.Valid && verifiedName.String != "" {
		return verifiedName.String, true
	}
	if pushName.Valid && pushName.String != "" {
		return pushName.String, true
	}
	if phone.Valid && phone.String != "" {
		return "+" + phone.String, true
	}
	return jid, true
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
