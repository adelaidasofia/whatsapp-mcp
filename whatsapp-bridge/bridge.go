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
	cfg    *Config
	db     *sql.DB
	client *whatsmeow.Client

	mu            sync.RWMutex
	connected     bool
	authenticated bool
	deviceJID     string
	lastSyncTime  time.Time
}

// NewBridge builds the whatsmeow client, prepares its session store, and registers event handlers.
// It does NOT connect yet; call Connect().
func NewBridge(ctx context.Context, cfg *Config, db *sql.DB, dbKey string) (*Bridge, error) {
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
		cfg:    cfg,
		db:     db,
		client: client,
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

	// Insert message.
	_, err = b.db.Exec(`
		INSERT INTO messages (id, chat_jid, sender_jid, sender_display, timestamp, type, content_text, content_normalized, is_from_me, scrubbed_text, scrub_flags_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, id, chatJID, senderJID, senderDisplay, ts, msgType, content, normalized,
		boolToInt(evt.Info.IsFromMe), scrubbed, ScrubFlagsJSON(flags))
	if err != nil {
		log.Printf("onMessage: message insert failed: %v", err)
	}

	// Upsert contact row for the sender (non-group messages; group participants sync separately).
	if !evt.Info.IsGroup {
		_, err = b.db.Exec(`
			INSERT INTO contacts (jid, phone, push_name, normalized_name, is_business, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(jid) DO UPDATE SET
				push_name = excluded.push_name,
				normalized_name = excluded.normalized_name,
				updated_at = excluded.updated_at
		`, senderJID, evt.Info.Sender.User, senderDisplay, Normalize(senderDisplay), 0, ts, ts)
		if err != nil {
			log.Printf("onMessage: contact upsert failed: %v", err)
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
