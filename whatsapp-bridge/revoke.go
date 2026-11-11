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
// Returns whether a stored message was cleared.
func (b *Bridge) applyRevoke(chatJID string, isGroup, revokerFromMe bool, targetID string, ts int64) bool {
	if targetID == "" || chatJID == "" {
		return false
	}
	var (
		storedChat string
		fromMe     int
		deleted    int
		mediaPath  sql.NullString
	)
	err := b.db.QueryRow(`SELECT chat_jid, is_from_me, is_deleted, media_path FROM messages WHERE id = ?`, targetID).
		Scan(&storedChat, &fromMe, &deleted, &mediaPath)
	if errors.Is(err, sql.ErrNoRows) {
		b.storeRevokeTombstone(chatJID, targetID, ts)
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

	// The deletion is the chat's newest activity, never an older one, and the
	// preview stops showing the message when it was the last one.
	if _, err := b.db.Exec(`
		UPDATE chats
		   SET last_message_time = MAX(COALESCE(last_message_time, 0), ?),
		       last_message_preview = CASE WHEN last_message_id = ? THEN NULL ELSE last_message_preview END,
		       updated_at = ?
		 WHERE jid = ?
	`, ts, targetID, time.Now().Unix(), storedChat); err != nil {
		log.Printf("revoke: chat update failed: %v", err)
	}
	log.Printf("revoke: a message deleted for everyone was cleared")
	return true
}

// storeRevokeTombstone keeps the fact that targetID was deleted before the
// bridge ever stored it, so a later delivery of it is not written with text.
func (b *Bridge) storeRevokeTombstone(chatJID, targetID string, ts int64) {
	// messages.chat_jid has a foreign key to chats.
	if _, err := b.db.Exec(`
		INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(jid) DO NOTHING
	`, chatJID, chatTypeFromJIDString(chatJID), ts, ts); err != nil {
		log.Printf("revoke: chat row for a tombstone failed: %v", err)
		return
	}
	if _, err := b.db.Exec(`
		INSERT INTO messages (id, chat_jid, timestamp, type, is_deleted)
		VALUES (?, ?, ?, 'system', 1)
		ON CONFLICT(id) DO NOTHING
	`, targetID, chatJID, ts); err != nil {
		log.Printf("revoke: tombstone failed: %v", err)
	}
}

// isDeleted reports whether id is a message deleted for everyone.
func (b *Bridge) isDeleted(id string) bool {
	var deleted int
	return b.db.QueryRow(`SELECT is_deleted FROM messages WHERE id = ?`, id).Scan(&deleted) == nil && deleted == 1
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
