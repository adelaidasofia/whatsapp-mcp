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

	updated := 0
	scanned := 0
	mediaSeen := 0
	decoded := 0
	for _, conv := range convs {
		log.Printf("history_sync: conversation %s has %d messages", conv.GetID(), len(conv.GetMessages()))

		// Track the OLDEST message in this conversation's chunk. It becomes
		// the anchor for the next step backwards (backfill_walk.go); without
		// it the walk would re-request the same window forever.
		var oldest chunkAnchor

		for _, hsMsg := range conv.GetMessages() {
			scanned++
			wm := hsMsg.GetMessage()
			if wm == nil {
				continue
			}
			key := wm.GetKey()
			if key == nil || key.GetID() == "" {
				continue
			}
			if ts := int64(wm.GetMessageTimestamp()); ts > 0 && (oldest.TS == 0 || ts < oldest.TS) {
				oldest = chunkAnchor{ID: key.GetID(), TS: ts, FromMe: key.GetFromMe()}
			}

			// Re-decode rows the pre-MYC-3284 decoder stored empty. Runs BEFORE
			// the media branch and must not share its `continue`: a recoverable
			// row is usually not media-bearing at all — the common case is text
			// that arrived inside an envelope the old switch could not see.
			if n, err := b.backfillDecodedContent(key.GetID(), wm.GetMessage()); err != nil {
				log.Printf("history_sync: content backfill %s failed: %v", key.GetID(), err)
			} else {
				decoded += n
			}

			fields, ok := extractFromMessage(wm.GetMessage())
			if !ok || len(fields.MediaKey) == 0 {
				continue
			}
			mediaSeen++

			// Only fill in fields that are currently NULL. This protects
			// rows where the live receive path already captured them
			// (post-patch messages). It also makes overlapping HistorySync
			// chunks idempotent.
			res, err := b.db.Exec(`
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
			n, _ := res.RowsAffected()
			updated += int(n)
		}

		// Decide whether this chat keeps walking backwards. Every stop
		// condition lives in the walker, so delivery just reports where the
		// chunk ended and obeys the verdict.
		if conv.GetID() != "" {
			b.continueWalk(context.Background(), conv.GetID(), oldest)
		}
	}
	log.Printf("history_sync: scanned %d msgs across %d conversations (progress=%d), saw %d media-bearing, backfilled %d media rows, re-decoded %d empty rows",
		scanned, len(convs), chunk.GetProgress(), mediaSeen, updated, decoded)
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
	//
	// Note this reaches only the most recent `count` messages, and re-calling
	// it returns the SAME window every time. Walking further back is
	// RequestHistoryBefore's job — see backfill_walk.go.
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

	resp, err = b.RequestHistoryBefore(ctx, chatJID, msgID, ts, isFromMe == 1, count)
	return
}

// RequestHistoryBefore asks WhatsApp for the `count` messages immediately
// before an EXPLICIT anchor, rather than before the newest row we hold.
//
// This is what makes walking backwards possible. RequestChatHistory always
// anchors on the newest message, so calling it repeatedly returns the same
// most-recent window forever — measured live 2026-08-01: a second sweep of 40
// chats delivered 8 more chunks and recovered 0 additional rows. Feeding the
// OLDEST message of each delivered chunk back in as the next anchor is what
// actually advances through a chat's history.
func (b *Bridge) RequestHistoryBefore(ctx context.Context, chatJID, anchorID string, anchorTS int64, anchorFromMe bool, count int) (resp whatsmeow.SendResponse, err error) {
	if !b.IsConnected() {
		err = errors.New("bridge not connected; cannot request history")
		return
	}
	if anchorID == "" {
		err = errors.New("anchor message id required")
		return
	}
	if count <= 0 {
		count = 50
	}
	if count > 200 {
		count = 200
	}

	msgID, ts := anchorID, anchorTS
	isFromMe := 0
	if anchorFromMe {
		isFromMe = 1
	}

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

// --- MYC-3284 content backfill ---------------------------------------------

// backfillDecodedContent re-decodes one history-sync message and repairs the
// stored row if — and only if — that row was written empty by the pre-MYC-3284
// decoder.
//
// The WHERE clause is the entire safety argument, so it is worth stating
// plainly. A row is eligible only when BOTH hold:
//
//	type = 'system'                          the old catch-all bucket
//	content_text IS NULL OR content_text=''  it actually has no payload
//
// So this can only ever turn a blank into something. It cannot overwrite text,
// cannot touch a media row, and cannot disturb a row the current decoder wrote
// (those carry either real text or an "[unsupported: …]" marker, and a marker
// is non-empty). Overlapping history chunks are therefore idempotent, and a
// stale re-delivery cannot clobber a fresher live row.
//
// A genuinely textless protocol carrier re-decodes to ("", "system") and the
// UPDATE is a no-op write of identical values, which is the correct outcome:
// key-distribution rows stay silent rather than gaining vault noise.
func (b *Bridge) backfillDecodedContent(msgID string, m *waE2E.Message) (int, error) {
	if msgID == "" || m == nil {
		return 0, nil
	}

	// Same decoder as live receive (see extractContentFromProto), so a
	// backfilled row is byte-identical to what it would have been had the
	// message arrived today.
	text, msgType := extractContentFromProto(m)
	if text == "" {
		// Nothing recovered — leave the row exactly as it is rather than
		// rewriting it with the same emptiness.
		return 0, nil
	}

	scrubbed, flags := Scrub(text)
	res, err := b.db.Exec(`
		UPDATE messages
		   SET type               = ?,
		       content_text       = ?,
		       content_normalized = ?,
		       scrubbed_text      = ?,
		       scrub_flags_json   = ?
		 WHERE id = ?
		   AND type = 'system'
		   AND (content_text IS NULL OR content_text = '')
	`, msgType, text, Normalize(text), scrubbed, ScrubFlagsJSON(flags), msgID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountEmptySystemRows reports how many rows still carry the pre-MYC-3284
// shape. The backfill's progress is measured by watching this fall.
func (b *Bridge) CountEmptySystemRows(ctx context.Context) (int, error) {
	var n int
	err := b.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM messages
		 WHERE type = 'system' AND (content_text IS NULL OR content_text = '')
	`).Scan(&n)
	return n, err
}

// SweepDecodeBackfill asks WhatsApp to re-deliver history for the chats holding
// the most empty rows. The repair itself happens asynchronously in
// processHistorySyncEvent as those chunks arrive, which is why this reports
// what it REQUESTED plus the current outstanding count rather than "fixed N" —
// a number it cannot honestly know yet.
//
// status@broadcast is excluded: WhatsApp does not answer on-demand history
// requests for it (verified live — the request sends, no chunk ever arrives),
// so including it burns request budget for nothing.
func (b *Bridge) SweepDecodeBackfill(ctx context.Context, maxChats, perChat, walkBudget, maxRounds int) (requested int, skipped int, remaining int, err error) {
	if !b.IsConnected() {
		return 0, 0, 0, errors.New("bridge not connected; cannot request history")
	}
	if maxChats <= 0 {
		maxChats = 20
	}
	if perChat <= 0 {
		perChat = 100
	}

	remaining, err = b.CountEmptySystemRows(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count empty rows: %w", err)
	}

	rows, qErr := b.db.QueryContext(ctx, `
		SELECT chat_jid, COUNT(*) AS n
		  FROM messages
		 WHERE type = 'system' AND (content_text IS NULL OR content_text = '')
		   AND chat_jid <> 'status@broadcast'
		 GROUP BY chat_jid
		 ORDER BY n DESC
		 LIMIT ?
	`, maxChats)
	if qErr != nil {
		return 0, 0, remaining, fmt.Errorf("select affected chats: %w", qErr)
	}
	defer rows.Close()

	var targets []string
	for rows.Next() {
		var jid string
		var n int
		if scanErr := rows.Scan(&jid, &n); scanErr != nil {
			return 0, 0, remaining, fmt.Errorf("scan affected chat: %w", scanErr)
		}
		targets = append(targets, jid)
	}
	if rErr := rows.Err(); rErr != nil {
		return 0, 0, remaining, fmt.Errorf("iterate affected chats: %w", rErr)
	}

	// Re-arm the walker for this sweep: fresh per-chat state and a fresh
	// global request budget. Also release any chat left pinned by an
	// unanswered request from a previous sweep.
	b.walker.sweepStale()
	b.walker.reset(walkBudget, maxRounds, perChat)

	for _, jid := range targets {
		// Anchor the FIRST request on the newest message, then register the
		// chat as walking. begin() returns false when the global budget is
		// already spent, in which case no request is issued at all.
		var anchorTS int64
		if tsErr := b.db.QueryRowContext(ctx,
			`SELECT timestamp FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 1`, jid,
		).Scan(&anchorTS); tsErr != nil {
			log.Printf("decode backfill: no anchor for %s: %v", jid, tsErr)
			skipped++
			continue
		}
		if !b.walker.begin(jid, anchorTS, perChat) {
			skipped++
			continue
		}
		if _, _, reqErr := b.RequestChatHistory(ctx, jid, perChat); reqErr != nil {
			// One chat failing (left group, deleted, unparseable JID) must not
			// abort the sweep for the rest.
			log.Printf("decode backfill: history request for %s failed: %v", jid, reqErr)
			skipped++
			continue
		}
		requested++
	}
	log.Printf("decode backfill: requested history for %d chats (%d skipped); %d empty rows outstanding",
		requested, skipped, remaining)
	return requested, skipped, remaining, nil
}
