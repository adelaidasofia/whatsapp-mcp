package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Bridge wraps a whatsmeow.Client and writes WhatsApp events into our SQLite database.
//
// Session state (whatsmeow's own sqlstore, holding encryption keys and device identity)
// lives in a separate SQLite file, both encrypted under the same SQLCipher key as the
// message database. The split follows whatsmeow's upstream pattern; we just share the key.
type Bridge struct {
	cfg         *Config
	db          *sql.DB
	client      *whatsmeow.Client
	transcriber *Transcriber

	mu            sync.RWMutex
	connected     bool
	authenticated bool
	deviceJID     string
	lastSyncTime  time.Time
}

// NewBridge builds the whatsmeow client, prepares its session store, and registers event handlers.
// It does NOT connect yet; call Connect().
// If transcriber is non-nil, voice-note messages are enqueued for transcription automatically.
func NewBridge(ctx context.Context, cfg *Config, db *sql.DB, dbKey string, transcriber *Transcriber) (*Bridge, error) {
	sessionDir := filepath.Dir(cfg.DBPath)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir session dir: %w", err)
	}
	sessionPath := filepath.Join(sessionDir, "session.db")

	dsn := fmt.Sprintf("file:%s?_foreign_keys=on", sessionPath)
	if cfg.EncryptDB {
		dsn = fmt.Sprintf("file:%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096&_foreign_keys=on",
			sessionPath, dbKey)
	}

	dbLog := waLog.Stdout("whatsmeow-store", "WARN", true)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, dbLog)
	if err != nil {
		return nil, fmt.Errorf("whatsmeow sqlstore init: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get first device: %w", err)
	}

	clientLog := waLog.Stdout("whatsmeow-client", "WARN", true)
	client := whatsmeow.NewClient(device, clientLog)

	b := &Bridge{
		cfg:         cfg,
		db:          db,
		client:      client,
		transcriber: transcriber,
	}
	client.AddEventHandler(b.handleEvent)
	return b, nil
}

// Connect establishes the WhatsApp connection. On first run (no stored device), it prints
// a terminal QR code for the user to scan with their phone. On subsequent runs it reconnects
// using the persisted device identity.
func (b *Bridge) Connect(ctx context.Context) error {
	if b.client.Store.ID == nil {
		// First run: pairing required.
		qrChan, err := b.client.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("qr channel: %w", err)
		}
		if err := b.client.Connect(); err != nil {
			return fmt.Errorf("connect for pairing: %w", err)
		}
		log.Println("waiting for QR code scan from phone (WhatsApp > Settings > Linked Devices > Link a Device)")
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				fmt.Println()
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				fmt.Println()
				log.Println("QR code printed above; scan it with your phone")
			case "success":
				log.Println("pairing success")
			case "timeout":
				return fmt.Errorf("qr pairing timed out; restart the bridge to get a fresh code")
			default:
				log.Printf("pairing event: %s", evt.Event)
			}
		}
	} else {
		// Returning user: reconnect with persisted identity.
		if err := b.client.Connect(); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
	}

	b.mu.Lock()
	b.connected = true
	b.authenticated = b.client.Store.ID != nil
	if b.client.Store.ID != nil {
		b.deviceJID = b.client.Store.ID.String()
	}
	b.mu.Unlock()

	log.Printf("bridge connected; device=%s", b.DeviceJID())
	return nil
}

func (b *Bridge) Disconnect() {
	if b.client != nil {
		b.client.Disconnect()
	}
	b.mu.Lock()
	b.connected = false
	b.authenticated = false
	b.mu.Unlock()
}

// Status returns current connection/auth state for the /api/status handler.
func (b *Bridge) Status() (connected, authed bool, deviceJID string, lastSync int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var ls int64
	if !b.lastSyncTime.IsZero() {
		ls = b.lastSyncTime.Unix()
	}
	return b.connected, b.authenticated, b.deviceJID, ls
}

func (b *Bridge) DeviceJID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.deviceJID
}

// Client returns the underlying whatsmeow client. Used by the backfiller
// to call client.Download against reconstituted AudioMessages. Read-only
// in spirit, but the whatsmeow client itself is not.
func (b *Bridge) Client() *whatsmeow.Client {
	return b.client
}

// IsConnected returns true when the bridge has an active whatsmeow connection
// and an authenticated device. Used by the send flow to refuse confirmations
// when the bridge is offline.
func (b *Bridge) IsConnected() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.connected && b.authenticated
}

// --- Event dispatch --------------------------------------------------------

func (b *Bridge) handleEvent(raw interface{}) {
	switch evt := raw.(type) {
	case *events.Message:
		b.onMessage(evt)
	case *events.Receipt:
		b.onReceipt(evt)
	case *events.Connected:
		b.mu.Lock()
		b.connected = true
		b.mu.Unlock()
		log.Println("whatsmeow: connected")
	case *events.Disconnected:
		b.mu.Lock()
		b.connected = false
		b.mu.Unlock()
		log.Println("whatsmeow: disconnected")
	case *events.LoggedOut:
		b.mu.Lock()
		b.connected = false
		b.authenticated = false
		b.mu.Unlock()
		log.Printf("whatsmeow: logged out; reason=%s (re-pair by restarting the bridge)", evt.Reason)
	case *events.PairSuccess:
		b.mu.Lock()
		b.authenticated = true
		if evt.ID.String() != "" {
			b.deviceJID = evt.ID.String()
		}
		b.mu.Unlock()
		log.Printf("whatsmeow: paired successfully; device=%s", evt.ID.String())
	case *events.HistorySync:
		b.mu.Lock()
		b.lastSyncTime = time.Now()
		b.mu.Unlock()
		log.Printf("whatsmeow: history sync chunk (progress=%d, conversations=%d)",
			evt.Data.GetProgress(), len(evt.Data.GetConversations()))
	case *events.CallOffer:
		b.onCallOffer(evt)
	case *events.CallTerminate:
		b.onCallTerminate(evt)
	}
}

// onMessage persists an incoming or outgoing message.
// The original content text is stored as-is; the prompt-injection-scrubbed representation
// is stored in scrubbed_text (plus flags in scrub_flags_json) for Claude to consume.
func (b *Bridge) onMessage(evt *events.Message) {
	chatJID := evt.Info.Chat.String()
	senderJID := evt.Info.Sender.String()
	id := evt.Info.ID
	ts := evt.Info.Timestamp.Unix()

	content, msgType := extractContent(evt)
	normalized := Normalize(content)
	scrubbed, flags := Scrub(content)

	senderDisplay := evt.Info.PushName
	if senderDisplay == "" {
		senderDisplay = evt.Info.Sender.User
	}

	// Upsert chat row (minimal; full chat sync happens via HistorySync).
	_, err := b.db.Exec(`
		INSERT INTO chats (jid, chat_type, name, normalized_name, created_at, updated_at, last_message_id, last_message_time, last_message_preview)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			last_message_id = excluded.last_message_id,
			last_message_time = excluded.last_message_time,
			last_message_preview = excluded.last_message_preview,
			updated_at = excluded.updated_at
	`, chatJID, chatTypeFromJID(evt.Info.Chat), senderDisplay, Normalize(senderDisplay),
		ts, ts, id, ts, truncate(content, 120))
	if err != nil {
		log.Printf("onMessage: chat upsert failed: %v", err)
	}

	// Capture re-download fields up front for any media-bearing message
	// (image/video/document/audio/sticker), so we can refetch later via
	// whatsmeow.Client.Download. Without these, image/document content is
	// only available at the moment of receipt — any consumer that wakes up
	// later (receipts pipeline, vision OCR, vault export with attachments)
	// has no way to recover the bytes. See media_download.go.
	mfields, _ := extractDownloadableFields(evt)

	// Insert message. Media columns are populated for image/video/document/
	// audio/sticker; for text/system/reaction etc. they go in as NULL.
	_, err = b.db.Exec(`
		INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, content_normalized, is_from_me, scrubbed_text, scrub_flags_json,
			media_key, media_direct_path, media_url, media_enc_sha256, media_sha256, media_file_length, media_key_timestamp, media_mime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, id, chatJID, senderJID, senderDisplay, ts, msgType, content, normalized,
		boolToInt(evt.Info.IsFromMe), scrubbed, ScrubFlagsJSON(flags),
		mfields.MediaKey, mfields.MediaDirectPath, mfields.MediaURL,
		mfields.MediaEncSHA, mfields.MediaSHA, mfields.MediaFileLength,
		mfields.MediaKeyTimestamp, mfields.MediaMime)
	if err != nil {
		log.Printf("onMessage: message insert failed: %v", err)
	}

	// Upsert contact row for the sender (non-group messages; group participants sync separately).
	//
	// Phone-column rule: only store `evt.Info.Sender.User` as phone when the
	// JID is the @s.whatsapp.net (DefaultUserServer) form, where User IS a
	// phone number. For @lid (HiddenUserServer) JIDs, User is an opaque LID
	// number, NOT a phone. Storing it as phone produces the bug class where
	// search and CRM lookups by phone silently miss the LID-form row. Prefer
	// SenderAlt when it's the phone form; otherwise leave phone NULL and let
	// the startup BackfillJIDAliases repair it once whatsmeow's LID store
	// learns the mapping.
	if !evt.Info.IsGroup {
		var phoneToStore sql.NullString
		switch {
		case evt.Info.Sender.Server == types.DefaultUserServer:
			phoneToStore = sql.NullString{String: evt.Info.Sender.User, Valid: true}
		case !evt.Info.SenderAlt.IsEmpty() && evt.Info.SenderAlt.Server == types.DefaultUserServer:
			phoneToStore = sql.NullString{String: evt.Info.SenderAlt.User, Valid: true}
		}
		_, err = b.db.Exec(`
			INSERT INTO contacts (jid, phone, push_name, normalized_name, is_business, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(jid) DO UPDATE SET
				push_name = excluded.push_name,
				normalized_name = excluded.normalized_name,
				phone = COALESCE(excluded.phone, contacts.phone),
				updated_at = excluded.updated_at
		`, senderJID, phoneToStore, senderDisplay, Normalize(senderDisplay), 0, ts, ts)
		if err != nil {
			log.Printf("onMessage: contact upsert failed: %v", err)
		}

		// Record JID alias edges. WhatsApp now exposes most contacts as a
		// (LID, phone-JID) pair; whatsmeow surfaces the alt form via
		// SenderAlt for incoming DMs and RecipientAlt for outgoing. Without
		// this, the two forms accumulate as orphaned rows and search/list
		// returns whichever the bridge happened to see first. See aliases.go.
		ctx := context.Background()
		if evt.Info.IsFromMe {
			recordJIDAlias(ctx, b.db, evt.Info.Chat, evt.Info.RecipientAlt, "message_recipient", ts)
		} else {
			recordJIDAlias(ctx, b.db, evt.Info.Sender, evt.Info.SenderAlt, "message_sender", ts)
		}
	}

	// Enqueue voice-note transcription. Fire-and-forget; the transcriber
	// writes back to messages.voice_note_transcript when done.
	if b.transcriber != nil && (msgType == "voice" || msgType == "audio") {
		if audio := evt.Message.GetAudioMessage(); audio != nil {
			audioMsg := audio // closure capture
			client := b.client
			b.transcriber.Enqueue(transcriptionJob{
				MessageID: id,
				MimeType:  audio.GetMimetype(),
				AudioDownloader: func(ctx context.Context) ([]byte, error) {
					return client.Download(ctx, audioMsg)
				},
			})
		}
	}
}

func (b *Bridge) onReceipt(evt *events.Receipt) {
	// Receipts are informational for now; we log them but don't alter state.
	// Future: update read-status on message rows if useful for list_messages output.
	_ = evt
}

func (b *Bridge) onCallOffer(evt *events.CallOffer) {
	if !b.cfg.CaptureCalls {
		return
	}
	chatJID := evt.CallCreator.String()
	_, err := b.db.Exec(`
		INSERT INTO calls (id, chat_jid, caller_jid, timestamp, call_type, is_group, is_outbound, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, evt.CallID, chatJID, evt.CallCreator.String(), evt.Timestamp.Unix(), "voice", 0, 0, "offered")
	if err != nil {
		log.Printf("onCallOffer: insert failed: %v", err)
	}
}

func (b *Bridge) onCallTerminate(evt *events.CallTerminate) {
	if !b.cfg.CaptureCalls {
		return
	}
	result := strings.ToLower(string(evt.Reason))
	if result == "" {
		result = "ended"
	}
	_, err := b.db.Exec(`
		UPDATE calls SET result = ? WHERE id = ?
	`, result, evt.CallID)
	if err != nil {
		log.Printf("onCallTerminate: update failed: %v", err)
	}
}

// --- Helpers ---------------------------------------------------------------

func chatTypeFromJID(j types.JID) string {
	switch j.Server {
	case types.GroupServer:
		return "group"
	case types.BroadcastServer:
		return "broadcast"
	case types.NewsletterServer:
		return "broadcast"
	default:
		return "direct"
	}
}

func extractContent(evt *events.Message) (text, msgType string) {
	m := evt.Message
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), "text"
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), "text"
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), "image"
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption(), "video"
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "", "voice"
		}
		return "", "audio"
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption(), "document"
	case m.GetStickerMessage() != nil:
		return "", "sticker"
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetComment(), "location"
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetDisplayName(), "contact"
	case m.GetReactionMessage() != nil:
		return m.GetReactionMessage().GetText(), "reaction"
	default:
		return "", "system"
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
