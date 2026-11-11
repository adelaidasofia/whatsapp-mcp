// revoke.go — a message its sender deleted "for everyone" is gone from the
// store (mycelium-hq/tickets#5656).
//
// WhatsApp delivers a delete-for-everyone as its own message: a
// ProtocolMessage of type REVOKE whose Key names the message taken back
// (whatsmeow's Client.BuildRevoke builds exactly this shape to send one).
// History sync carries the same fact a second way: the revoked message itself,
// re-delivered with messageStubType REVOKE and no content.
//
// Before this file the bridge had an is_deleted column that nothing set. A
// revoke fell through extractContent's default branch and was stored as a row
// of its own, "[unsupported: protocolMessage]", while the message it took back
// kept its text, its transcript and its media, and every reader went on
// serving it: /api/messages, the vault export, and from there Mycelium, which
// keeps a client's chat in the company's vault.
//
// A revoke now does what the person who sent it asked for. The stored message
// keeps only what says a message was there (id, chat, sender, time): its
// text, normalized and scrubbed copies, transcript, media keys and any file
// downloaded for it are cleared, and is_deleted is set, so every reader skips
// it. The chat's last_message_time moves to the revoke, because a deletion is
// activity in the chat (WhatsApp itself shows the line changing), and that is
// what tells the vault export and Mycelium's watcher to read the chat again.
//
// A revoke for a message the bridge has not stored yet (history arriving out
// of order) leaves a tombstone row under that id, so the message cannot be
// written later with its text: every insert path either does nothing on an
// existing id or upgrades only an undecryptable placeholder, which a tombstone
// is not.

package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
)

// revokedMessageID returns the id of the message a live message revokes, or ""
// when it is not a revoke.
func revokedMessageID(m *waE2E.Message) string {
	pm := unwrapEnvelope(m).GetProtocolMessage()
	if pm.GetType() != waE2E.ProtocolMessage_REVOKE {
		return ""
	}
	return pm.GetKey().GetID()
}

// revokedHistoryID returns the id of the message a history-sync entry says was
// revoked: the entry itself when it is a revoke stub, or the message a revoke
// carried in history names. "" when it is neither.
func revokedHistoryID(wm *waWeb.WebMessageInfo) string {
	if wm.GetMessageStubType() == waWeb.WebMessageInfo_REVOKE {
		return wm.GetKey().GetID()
	}
	return revokedMessageID(wm.GetMessage())
}

// applyRevoke clears the message targetID from the store. chatJID is the chat
// the revoke arrived in; revokerFromMe says whether this account sent it.
//
// The message must be in the same chat (any of its JID aliases). In a direct
// chat it must also be from the same side as the revoke: only its own sender
// can take a message back there, so a revoke from the other side naming one of
// ours is ignored. In a group an admin can delete anyone's message, so any
// revoke in the chat counts.
//
// In a broadcast chat (status, a list) the chat is shared by every contact, so
// a live revoke must also come from the message's own sender. revokerJID is ""
// for history, which reports a deletion rather than asking for one.
//
// Returns whether a stored message was cleared.
func (b *Bridge) applyRevoke(chatJID string, isGroup, revokerFromMe bool, revokerJID, targetID string, ts int64) bool {
	if targetID == "" || chatJID == "" {
		return false
	}
	var (
		storedChat string
		fromMe     int
		deleted    int
		mediaPath  sql.NullString
		sender     sql.NullString
	)
	broadcast := chatTypeFromJIDString(chatJID) == "broadcast"
	err := b.db.QueryRow(`SELECT chat_jid, is_from_me, is_deleted, media_path, sender_jid FROM messages WHERE id = ?`, targetID).
		Scan(&storedChat, &fromMe, &deleted, &mediaPath, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		// A broadcast tombstone could not be checked against its sender later.
		if !broadcast {
			b.storeRevokeTombstone(chatJID, revokerFromMe, targetID, ts)
		}
		return false
	}
	if err != nil {
		log.Printf("revoke: lookup failed: %v", err)
		return false
	}
	if deleted == 1 {
		return false
	}
	if !b.sameChat(chatJID, storedChat) {
		log.Printf("revoke: ignored, the message it names is in another chat")
		return false
	}
	if !isGroup && (fromMe == 1) != revokerFromMe {
		log.Printf("revoke: ignored, a direct chat's message can only be taken back by its sender")
		return false
	}
	if broadcast && revokerJID != "" && !revokerFromMe && !b.sameChat(revokerJID, sender.String) {
		log.Printf("revoke: ignored, a broadcast message can only be taken back by its sender")
		return false
	}

	res, err := b.db.Exec(`
		UPDATE messages
		   SET is_deleted = 1,
		       type = 'system',
		       content_text = NULL,
		       content_normalized = NULL,
		       scrubbed_text = NULL,
		       scrub_flags_json = NULL,
		       raw_type = NULL,
		       reactions_json = NULL,
		       voice_note_transcript = NULL,
		       voice_note_transcript_backend = NULL,
		       voice_note_transcript_at = NULL,
		       media_path = NULL,
		       media_mime = NULL,
		       media_duration_sec = NULL,
		       media_key = NULL,
		       media_direct_path = NULL,
		       media_url = NULL,
		       media_enc_sha256 = NULL,
		       media_sha256 = NULL,
		       media_file_length = NULL,
		       media_key_timestamp = NULL
		 WHERE id = ? AND is_deleted = 0
	`, targetID)
	if err != nil {
		log.Printf("revoke: clearing the message failed: %v", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	if _, err := b.db.Exec(`DELETE FROM media_downloads WHERE message_id = ?`, targetID); err != nil {
		log.Printf("revoke: forgetting the downloaded file failed: %v", err)
	}
	b.removeMediaFile(mediaPath)

	// The deletion is the chat's newest activity, never an older one.
	if _, err := b.db.Exec(`
		UPDATE chats
		   SET last_message_time = MAX(COALESCE(last_message_time, 0), ?),
		       updated_at = ?
		 WHERE jid = ?
	`, ts, time.Now().Unix(), storedChat); err != nil {
		log.Printf("revoke: chat update failed: %v", err)
	}
	b.refreshChatPreview(storedChat)
	log.Printf("revoke: a message deleted for everyone was cleared")
	return true
}

// refreshChatPreview sets a chat's preview from its newest message still
// stored. Every writer of last_message_preview (live receive, history sync,
// the Baileys import) wrote it from some message, not always naming which, so
// after a delete or an edit the only preview that is surely right is the one
// read back from what the chat holds now.
func (b *Bridge) refreshChatPreview(chatJID string) {
	var (
		id      sql.NullString
		content sql.NullString
	)
	err := b.db.QueryRow(`
		SELECT id, content_text FROM messages
		 WHERE chat_jid = ? AND is_deleted = 0
		 ORDER BY timestamp DESC, id DESC
		 LIMIT 1
	`, chatJID).Scan(&id, &content)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("revoke: reading the chat's newest message failed: %v", err)
		return
	}
	var preview sql.NullString
	if content.Valid && content.String != "" {
		preview = sql.NullString{String: truncate(content.String, 120), Valid: true}
	}
	if _, err := b.db.Exec(`UPDATE chats SET last_message_id = ?, last_message_preview = ? WHERE jid = ?`, id, preview, chatJID); err != nil {
		log.Printf("revoke: chat preview update failed: %v", err)
	}
}

// applyEdit writes an edit onto the message it names, which must be in the
// same chat and from the same sender (only a message's author can edit it).
// Returns false when the original is not stored, so the caller stores the edit
// under the original's id; true when the edit was applied or refused.
func (b *Bridge) applyEdit(chatJID string, isGroup, editorFromMe bool, editorJID, targetID string, evt *events.Message) bool {
	var (
		storedChat string
		fromMe     int
		deleted    int
		sender     sql.NullString
	)
	err := b.db.QueryRow(`SELECT chat_jid, is_from_me, is_deleted, sender_jid FROM messages WHERE id = ?`, targetID).
		Scan(&storedChat, &fromMe, &deleted, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		log.Printf("edit: lookup failed: %v", err)
		return true
	}
	if deleted == 1 {
		// Deleted already: the edit writes nothing, unless the tombstone is not
		// this message's (then it is dropped and the edit is stored).
		return b.landsOnTombstone(targetID, chatJID, isGroup, editorFromMe)
	}
	if !b.sameChat(chatJID, storedChat) || (fromMe == 1) != editorFromMe ||
		(!editorFromMe && chatTypeFromJIDString(chatJID) != "direct" && !b.sameChat(editorJID, sender.String)) {
		log.Printf("edit: ignored, only a message's author can edit it, in its own chat")
		return true
	}
	content, msgType := extractContent(evt)
	scrubbed, flags := Scrub(content)
	if _, err := b.db.Exec(`
		UPDATE messages
		   SET content_text = ?, content_normalized = ?, scrubbed_text = ?, scrub_flags_json = ?,
		       raw_type = ?, is_edited = 1
		 WHERE id = ? AND is_deleted = 0
	`, content, Normalize(content), scrubbed, ScrubFlagsJSON(flags), rawTypeNullable(msgType, content), targetID); err != nil {
		log.Printf("edit: update failed: %v", err)
		return true
	}
	b.refreshChatPreview(storedChat)
	return true
}

// storeRevokeTombstone keeps the fact that targetID was deleted before the
// bridge ever stored it, so a later delivery of it is not written with text.
// It records the chat and the revoker's side (is_from_me) and no sender, which
// is what marks it as a tombstone: landsOnTombstone checks a later delivery
// against them, as applyRevoke checks a stored message.
func (b *Bridge) storeRevokeTombstone(chatJID string, revokerFromMe bool, targetID string, ts int64) {
	// messages.chat_jid has a foreign key to chats.
	if _, err := b.db.Exec(`
		INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(jid) DO NOTHING
	`, chatJID, chatTypeFromJIDString(chatJID), ts, ts); err != nil {
		log.Printf("revoke: chat row for a tombstone failed: %v", err)
		return
	}
	if _, err := b.db.Exec(`
		INSERT INTO messages (id, chat_jid, timestamp, type, is_from_me, is_deleted)
		VALUES (?, ?, ?, 'system', ?, 1)
		ON CONFLICT(id) DO NOTHING
	`, targetID, chatJID, ts, boolToInt(revokerFromMe)); err != nil {
		log.Printf("revoke: tombstone failed: %v", err)
	}
}

// landsOnTombstone reports whether a message arriving now (in chatJID, from
// this account or not) is one a revoke already deleted, so nothing of it is
// written. A tombstone the message does not match (another chat, or in a
// direct chat a revoke from the other side) was not a valid revoke of it: the
// tombstone is dropped and the message is stored as usual.
func (b *Bridge) landsOnTombstone(id, chatJID string, isGroup, fromMe bool) bool {
	var (
		storedChat string
		tombFromMe int
		deleted    int
		sender     sql.NullString
	)
	if err := b.db.QueryRow(`SELECT chat_jid, is_from_me, is_deleted, sender_jid FROM messages WHERE id = ?`, id).
		Scan(&storedChat, &tombFromMe, &deleted, &sender); err != nil || deleted != 1 {
		return false
	}
	if sender.Valid {
		// A stored message that was cleared: it stays cleared.
		return true
	}
	if b.sameChat(chatJID, storedChat) && (isGroup || (tombFromMe == 1) == fromMe) {
		return true
	}
	if _, err := b.db.Exec(`DELETE FROM messages WHERE id = ? AND is_deleted = 1 AND sender_jid IS NULL`, id); err != nil {
		log.Printf("revoke: dropping a tombstone that does not match its message failed: %v", err)
		return true
	}
	log.Printf("revoke: a tombstone did not match the message that arrived and was dropped")
	return false
}

// sameChat reports whether two chat JIDs name the same chat, through the
// alias table (a direct chat is known by its phone JID and its LID).
func (b *Bridge) sameChat(a, c string) bool {
	if a == c {
		return true
	}
	aliases, err := resolveAliases(context.Background(), b.db, a)
	if err != nil {
		return false
	}
	for _, j := range aliases {
		if j == c {
			return true
		}
	}
	return false
}

// removeMediaFile deletes a file downloaded for a deleted message, but only
// inside the bridge's own media folder: the path came from the database, and
// nothing outside that folder is the bridge's to remove.
func (b *Bridge) removeMediaFile(p sql.NullString) {
	if !p.Valid || p.String == "" || b.cfg == nil || b.cfg.MediaPath == "" {
		return
	}
	root, err := filepath.Abs(b.cfg.MediaPath)
	if err != nil {
		return
	}
	file, err := filepath.Abs(p.String)
	if err != nil {
		return
	}
	if rel, err := filepath.Rel(root, file); err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		log.Printf("revoke: a deleted message's file is outside the media folder and was left alone")
		return
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("revoke: removing a deleted message's file failed: %v", err)
	}
}
