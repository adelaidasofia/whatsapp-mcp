package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

	// rootCtx is the process-lifetime context, kept so event handlers can
	// restart the login loop (e.g. after a WhatsApp-side logout) without
	// holding a request-scoped ctx.
	rootCtx context.Context

	// walker drives the MYC-3284 backfill's backwards walk through chat
	// history. See backfill_walk.go.
	walker *backfillWalker

	mu            sync.RWMutex
	connected     bool
	authenticated bool
	deviceJID     string
	lastSyncTime  time.Time

	// Auth lifecycle surfaced over /api/status + /api/auth/* (see auth.go).
	authState        AuthState
	currentQR        string
	qrExpiresAt      time.Time
	pairingCode      string
	loggedOutReason  string
	loginRunning     bool
	pairPhoneOnStart string // --pair-phone flag: request a typed code on first QR event
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
		rootCtx:     ctx,
		authState:   AuthStateUnauthenticated,
		walker:      newBackfillWalker(),
	}
	client.AddEventHandler(b.handleEvent)
	return b, nil
}

// Authentication (QR loop, pairing codes, re-login) lives in auth.go —
// main() runs RunAuth in a goroutine so the HTTP API is available during
// first-run pairing.

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
		if b.authenticated {
			b.authState = AuthStatePaired
		}
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
		b.authState = AuthStateLoggedOut
		b.loggedOutReason = fmt.Sprintf("%v", evt.Reason)
		b.currentQR = ""
		b.pairingCode = ""
		b.mu.Unlock()
		log.Printf("whatsmeow: logged out; reason=%v — starting a fresh pairing flow (scan the new QR, or POST /api/auth/pair-phone)", evt.Reason)
		// Re-enter pairing without a process restart. whatsmeow clears the
		// device store on logout; the small delay lets that settle.
		go func() {
			select {
			case <-b.rootCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if err := b.loginLoop(b.rootCtx); err != nil && b.rootCtx.Err() == nil {
				log.Printf("re-login after logout failed: %v (POST /api/auth/reconnect to retry)", err)
			}
		}()
	case *events.PairSuccess:
		b.mu.Lock()
		b.authenticated = true
		b.authState = AuthStatePaired
		b.currentQR = ""
		b.pairingCode = ""
		b.loggedOutReason = ""
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
		// Backfill media-key fields for any historical image/video/document/
		// sticker messages WhatsApp re-delivers. See history_sync.go.
		b.processHistorySyncEvent(evt)
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
				ChatJID:   chatJID,
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

// extractContent decodes a live-receive event.
func extractContent(evt *events.Message) (text, msgType string) {
	return extractContentFromProto(evt.Message)
}

// extractContentFromProto is the real decoder. Live receive reaches it through
// extractContent; the history-sync backfill reaches it directly, because
// HistorySync delivers the same waE2E.Message the live path sees.
//
// Both callers share this ONE function on purpose. A backfill carrying its own
// copy of the decode rules is free to drift from the live one, and the drift
// would be invisible: rows it rewrote would decode differently from rows
// arriving beside them, with nothing comparing the two.
func extractContentFromProto(raw *waE2E.Message) (text, msgType string) {
	// Reach the real payload first. Disappearing messages, view-once,
	// device-sent, document-with-caption and edits all nest the actual message
	// inside a wrapper, and every case below tests TOP-LEVEL fields only — so
	// without this, a plain text message sent in a chat with disappearing
	// messages enabled matches nothing and lands in the default branch. It
	// would be marked "[unsupported: ephemeralMessage]": visible, but with the
	// text still uncaptured. See content_decode.go.
	m := unwrapEnvelope(raw)
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
		// Second decode tier: types the switch above does not name but that do
		// carry user-visible text (polls, events, live location, contact
		// arrays, group invites, video notes, the commerce family). These were
		// showing up as markers on the live bridge — a marker is right for
		// something we cannot read, not for something we simply had not
		// decoded yet. See content_decode.go.
		//
		// The `t != ""` gate is the important part: a decoder only gets to
		// CLAIM a message when it actually produced text. A poll with no
		// question, or an event stripped of its fields, would otherwise be
		// stored as an empty row — trading this ticket's loud marker back for
		// the silent drop it replaced. Falling through keeps such a message
		// visible, which is always the safer of the two.
		if t, mt, ok := decodeExtraContent(m); ok && t != "" {
			return t, mt
		}

		// Fail LOUD (MYC-3284): a message that carries an undecoded CONTENT
		// field keeps a NON-EMPTY marker naming the real proto type, so it is
		// never a silent empty row that reads as "no text". A genuinely
		// content-free message (metadata only) stays a plain empty "system".
		//
		// The marker rides in content_text, NOT in a new `type` value: the
		// messages.type column has a CHECK constraint (migrations/001) that
		// admits a fixed set, and SQLite cannot widen a CHECK without a full
		// table rebuild — a disproportionate risk to a live message store just
		// to relabel a rare row. The marker is what every reader keys on
		// (unsupportedRawType), and it is queryable:
		//   SELECT * FROM messages WHERE content_text LIKE '[unsupported: %'
		if raw := unsupportedMessageType(m); raw != "" {
			log.Printf("extractContent: undecoded WhatsApp message type %q — stored with an explicit unsupported marker (no text captured; add a decoder if it carries user text)", raw)
			return unsupportedMarker(raw), "system"
		}
		return "", "system"
	}
}

// The content_text marker stored for an undecodable message. ONE declaration
// shared by the writers (extractContent, baileysExtractContent) and every
// reader (vault export, the /healthcheck by-type counts), so what is written
// and what is parsed back can never drift (MYC-3284).
const (
	unsupportedPrefix = "[unsupported: "
	unsupportedSuffix = "]"
)

// unsupportedMarker renders the content_text stored for an undecodable message.
func unsupportedMarker(rawType string) string {
	return unsupportedPrefix + rawType + unsupportedSuffix
}

// unsupportedRawType recovers the raw type from a stored marker, or "" when the
// text is not one (a decoded message, or a legacy pre-MYC-3284 row).
func unsupportedRawType(text string) string {
	if !strings.HasPrefix(text, unsupportedPrefix) || !strings.HasSuffix(text, unsupportedSuffix) {
		return ""
	}
	return text[len(unsupportedPrefix) : len(text)-len(unsupportedSuffix)]
}

// carrierFields are message fields that provably carry NO user-visible text,
// so a message containing only these is genuinely textless — not something the
// bridge failed to read. Marking them "unsupported" is technically honest but
// practically wrong: it writes a placeholder line into the member's chat files
// for cryptographic plumbing they never sent and cannot see. Measured on the
// live bridge after MYC-3284 shipped: 6 of the first 8 markers were
// senderKeyDistributionMessage (Signal-protocol group key distribution, which
// rides on its own when a group's keys rotate).
//
// This list is deliberately SHORT and admits only fields whose textlessness is
// certain. An UNKNOWN field still fails loud — that safety property is the
// whole point of MYC-3284 and is not weakened here. Anything uncertain (e.g.
// pinInChatMessage, a real member action) keeps its marker until it is properly
// decoded, so it stays visible rather than quietly disappearing.
var carrierFields = map[string]bool{
	// Rides alongside real content on many message types.
	"messageContextInfo": true,
	// Signal-protocol group sender-key distribution. No user payload, ever.
	"senderKeyDistributionMessage": true,
}

// unsupportedMessageType names the populated content field(s) of a message
// extractContent does not decode, so an undecodable-but-populated message is
// stored with a distinct, queryable marker instead of silently collapsing to an
// empty "system" row (MYC-3284). Fields in carrierFields are skipped; a message
// with no other populated field is genuinely textless and returns "" (the
// caller keeps its existing "system" classification). Deterministic (sorted)
// so the stored value and the WARN are stable.
func unsupportedMessageType(m proto.Message) string {
	if m == nil {
		return ""
	}
	r := m.ProtoReflect()
	if !r.IsValid() {
		return ""
	}
	var names []string
	r.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if name := string(fd.Name()); !carrierFields[name] {
			names = append(names, name)
		}
		return true
	})
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return strings.Join(names, ",")
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
