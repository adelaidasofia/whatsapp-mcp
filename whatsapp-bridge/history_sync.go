// history_sync.go — programmatic history-sync backfill for media keys.
//
// Background: the bridge originally lost media-key fields for image / video /
// document / sticker messages received before the 2026-05-08 patch (it only
// persisted them for audio). The follow-up patch fixes the live receive path,
// but does NOT recover historical messages that already exist in the messages
// table with NULL media-key fields.
//
// This file fixes the historical gap programmatically. WhatsApp's history-sync
// protocol delivers the same waE2E.Message protos that arrive via live receive,
// inside HistorySyncMsg wrappers attached to Conversation entries. Processing
// those events lets us backfill media-key fields without flipping the global
// AutoDownloadMedia flag, without asking the user to manually export a chat,
// and without storing duplicate media bytes anywhere.
//
// Two halves:
//
//   1. processHistorySyncEvent — invoked from the live event handler when
//      whatsmeow delivers a *events.HistorySync. Walks every Conversation's
//      messages, extracts media-key fields, and runs an UPDATE that only
//      writes when the existing row's media_key is NULL (so we don't clobber
//      good values with stale ones).
//
//   2. RequestChatHistory — finds the oldest message we have for a chat and
//      sends a peer HistorySyncOnDemandRequest asking WhatsApp for `count`
//      messages immediately before that one. WhatsApp delivers the response
//      asynchronously over the events.HistorySync stream, which #1 picks up.
//
// Net effect: a single POST /api/admin/request-history call against a chat
// triggers the backfill, and the receipts pipeline (or any other consumer)
// can immediately decrypt the historical media via the existing
// /api/media/download endpoint.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// extractFromMessage is the inner-type variant of extractDownloadableFields.
// Live receive (events.Message) wraps the proto in evt.Message; HistorySync
// delivery wraps it in HistorySyncMsg.GetMessage().GetMessage(). Both reach
// the same *waE2E.Message, so both paths share this helper.
func extractFromMessage(m *waE2E.Message) (mediaFields, bool) {
	if m == nil {
		return mediaFields{}, false
	}
	if mm := m.GetImageMessage(); mm != nil {
		return fieldsFrom(mm), true
	}
	if mm := m.GetVideoMessage(); mm != nil {
		return fieldsFrom(mm), true
	}
	if mm := m.GetDocumentMessage(); mm != nil {
		return fieldsFrom(mm), true
	}
	if mm := m.GetAudioMessage(); mm != nil {
		return fieldsFrom(mm), true
	}
	if mm := m.GetStickerMessage(); mm != nil {
		return fieldsFrom(mm), true
	}
	return mediaFields{}, false
}

// processHistorySyncEvent updates messages.media_key for any rows currently
// holding NULL keys when WhatsApp re-delivers them via history sync.
//
// COALESCE in the UPDATE makes this a no-op for rows that already have keys,
// so re-running on overlapping history chunks is safe.
func (b *Bridge) processHistorySyncEvent(evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	chunk := evt.Data
	convs := chunk.GetConversations()
	if len(convs) == 0 {
		return
	}

	// Our own JID, used to attribute is_from_me=1 messages to a sender. Empty
	// only if somehow unpaired mid-sync; then from-me rows get an empty sender.
	var ownJID string
	if id := b.client.Store.ID; id != nil {
		ownJID = id.ToNonAD().String()
	}

	inserted := 0
	updated := 0
	scanned := 0
	mediaSeen := 0
	for _, conv := range convs {
		chatJID := conv.GetID()
		msgs := conv.GetMessages()
		log.Printf("history_sync: conversation %s has %d messages", chatJID, len(msgs))
		if chatJID == "" {
			continue
		}

		// Newest message in this conversation, used to seed the chat row's
		// sort key + preview without regressing a live chat's fresher values.
		var convMaxTS int64
		var convPreview string
		for _, hsMsg := range msgs {
			wm := hsMsg.GetMessage()
			if wm == nil {
				continue
			}
			if ts := int64(wm.GetMessageTimestamp()); ts >= convMaxTS {
				convMaxTS = ts
				c, _ := extractContentFromProto(wm.GetMessage())
				convPreview = truncate(c, 120)
			}
		}

		// Ensure the chat row exists BEFORE inserting messages: messages.chat_jid
		// has a FOREIGN KEY to chats(jid) and the message DB runs with foreign_keys
		// ON, so a missing parent row would fail every insert. MAX() guards keep a
		// live chat's newer last_message_time from being clobbered by history.
		chatName := conv.GetName()
		if _, err := b.db.Exec(`
			INSERT INTO chats (jid, chat_type, name, normalized_name, last_message_time, last_message_preview, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(jid) DO UPDATE SET
				name              = COALESCE(chats.name, excluded.name),
				normalized_name   = COALESCE(chats.normalized_name, excluded.normalized_name),
				last_message_time = MAX(COALESCE(chats.last_message_time, 0), excluded.last_message_time),
				updated_at        = excluded.updated_at
		`, chatJID, chatTypeFromJIDString(chatJID), nullIfEmpty(chatName), nullIfEmpty(Normalize(chatName)),
			convMaxTS, nullIfEmpty(convPreview), convMaxTS, convMaxTS); err != nil {
			log.Printf("history_sync: chat upsert %s failed: %v", chatJID, err)
			continue
		}

		for _, hsMsg := range msgs {
			scanned++
			wm := hsMsg.GetMessage()
			if wm == nil {
				continue
			}
			key := wm.GetKey()
			if key == nil || key.GetID() == "" {
				continue
			}

			content, msgType := extractContentFromProto(wm.GetMessage())
			ts := int64(wm.GetMessageTimestamp())
			fromMe := key.GetFromMe()

			senderJID := chatJID
			switch {
			case fromMe:
				senderJID = ownJID
			case key.GetParticipant() != "":
				senderJID = key.GetParticipant()
			}
			senderDisplay := wm.GetPushName()
			if senderDisplay == "" {
				senderDisplay = senderJID
			}

			normalized := Normalize(content)
			scrubbed, flags := Scrub(content)
			mfields, _ := extractFromMessage(wm.GetMessage())

			// Insert the historical message. ON CONFLICT(id) DO NOTHING makes
			// overlapping history chunks (and re-pairing) idempotent, and leaves
			// any live-received row untouched. Columns mirror onMessage exactly,
			// so a backfilled row is indistinguishable from a live one.
			res, err := b.db.Exec(`
				INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, content_normalized, is_from_me, scrubbed_text, scrub_flags_json,
					media_key, media_direct_path, media_url, media_enc_sha256, media_sha256, media_file_length, media_key_timestamp, media_mime)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING
			`, key.GetID(), chatJID, senderJID, senderDisplay, ts, msgType, content, normalized,
				boolToInt(fromMe), scrubbed, ScrubFlagsJSON(flags),
				mfields.MediaKey, mfields.MediaDirectPath, mfields.MediaURL,
				mfields.MediaEncSHA, mfields.MediaSHA, mfields.MediaFileLength,
				mfields.MediaKeyTimestamp, mfields.MediaMime)
			if err != nil {
				log.Printf("history_sync: insert %s failed: %v", key.GetID(), err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted += int(n)
			}

			// For rows that already existed with NULL media keys (pre-patch live
			// receives), still backfill the media fields via COALESCE — the
			// file's original purpose. A no-op for the row we just inserted.
			fields, ok := extractFromMessage(wm.GetMessage())
			if !ok || len(fields.MediaKey) == 0 {
				continue
			}
			mediaSeen++
			ures, err := b.db.Exec(`
				UPDATE messages
				   SET media_key            = COALESCE(media_key, ?),
				       media_direct_path    = COALESCE(media_direct_path, ?),
				       media_url            = COALESCE(media_url, ?),
				       media_enc_sha256     = COALESCE(media_enc_sha256, ?),
				       media_sha256         = COALESCE(media_sha256, ?),
				       media_file_length    = COALESCE(media_file_length, ?),
				       media_key_timestamp  = COALESCE(media_key_timestamp, ?),
				       media_mime           = COALESCE(media_mime, ?)
				 WHERE id = ?
				   AND media_key IS NULL
			`,
				fields.MediaKey, fields.MediaDirectPath, fields.MediaURL,
				fields.MediaEncSHA, fields.MediaSHA, fields.MediaFileLength,
				fields.MediaKeyTimestamp, fields.MediaMime, key.GetID(),
			)
			if err != nil {
				log.Printf("history_sync: update %s failed: %v", key.GetID(), err)
				continue
			}
			n, _ := ures.RowsAffected()
			updated += int(n)
		}
	}
	log.Printf("history_sync: scanned %d msgs across %d conversations (progress=%d), inserted %d new, saw %d media-bearing, backfilled %d media rows",
		scanned, len(convs), chunk.GetProgress(), inserted, mediaSeen, updated)
}

// chatTypeFromJIDString parses a raw JID string and returns the chats.chat_type
// value. Falls back to "direct" when the JID cannot be parsed, so the CHECK
// constraint on chats.chat_type is always satisfied.
func chatTypeFromJIDString(jid string) string {
	j, err := types.ParseJID(jid)
	if err != nil {
		return "direct"
	}
	return chatTypeFromJID(j)
}

// nullIfEmpty maps "" to a NULL so nullable TEXT columns stay NULL rather than
// storing empty strings (keeps COALESCE-based upserts meaningful).
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// RequestChatHistory triggers WhatsApp to deliver `count` messages immediately
// before the oldest message we currently have for `chatJID`. The response
// arrives asynchronously as one or more events.HistorySync events, processed
// by processHistorySyncEvent.
//
// Returns the oldest known message id we anchored on, plus the SendResponse
// from whatsmeow so the operator can correlate.
//
// If no messages exist for the chat yet, returns an error — there's nothing
// to anchor the request on. (For brand-new chats, just wait for live receive.)
func (b *Bridge) RequestChatHistory(ctx context.Context, chatJID string, count int) (anchorID string, resp whatsmeow.SendResponse, err error) {
	if !b.IsConnected() {
		err = errors.New("bridge not connected; cannot request history")
		return
	}
	if count <= 0 {
		count = 50
	}
	if count > 200 {
		count = 200
	}

	// Find the NEWEST message we have for this chat. WhatsApp's
	// HistorySyncOnDemandRequest returns the `count` messages immediately
	// BEFORE the given anchor — so anchoring on the newest gives us the
	// historical chunk we already have rows for (but where media-key
	// fields are NULL because they weren't persisted at receive time).
	// WhatsApp re-delivers the full proto including media keys, and
	// processHistorySyncEvent fills them in via COALESCE.
	//
	// Earlier mistake: anchoring on the OLDEST message asks for pre-history
	// (messages before the chat started), which always returns zero results.
	var (
		msgID    string
		ts       int64
		isFromMe int
	)
	err = b.db.QueryRowContext(ctx, `
		SELECT id, timestamp, is_from_me
		FROM messages
		WHERE chat_jid = ?
		ORDER BY timestamp DESC
		LIMIT 1
	`, chatJID).Scan(&msgID, &ts, &isFromMe)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("no existing messages for chat %s; cannot anchor history request", chatJID)
			return
		}
		err = fmt.Errorf("query oldest message: %w", err)
		return
	}
	anchorID = msgID

	chat, parseErr := types.ParseJID(chatJID)
	if parseErr != nil {
		err = fmt.Errorf("invalid chat_jid: %w", parseErr)
		return
	}

	mi := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: isFromMe == 1,
		},
		ID:        msgID,
		Timestamp: time.Unix(ts, 0),
	}
	reqMsg := b.client.BuildHistorySyncRequest(mi, count)

	// Self-peer send. The HistorySyncOnDemandRequest is a peer-data-operation
	// message addressed to our own device; WhatsApp interprets it server-side
	// and replies via the history-sync stream. Setting Peer=true on the
	// SendRequestExtra is required for whatsmeow to route as a peer message.
	ownJID := b.client.Store.ID
	if ownJID == nil {
		err = errors.New("bridge has no device id; cannot send peer message")
		return
	}
	resp, err = b.client.SendMessage(ctx, ownJID.ToNonAD(), reqMsg, whatsmeow.SendRequestExtra{Peer: true})
	if err != nil {
		err = fmt.Errorf("send history-sync request: %w", err)
		return
	}
	log.Printf("history_sync: requested %d messages for %s before %s (ts=%d)",
		count, chatJID, msgID, ts)
	return
}

