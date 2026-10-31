package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The rules every outbound message passes before it leaves. They live here,
// in the bridge, because its loopback API has no authentication: a limit in
// the MCP server or in an app that runs the bridge binds only the callers
// that go through them, while anything on the machine can POST to
// /api/sends. Drafting stays free; the rules bind at confirm.
//
//  1. Never to a broadcast list, status or channel.
//  2. Only into a conversation that already exists: the chat, or another JID
//     of the same person, has a stored message. No first contact with
//     strangers.
//  3. Never the same text to several chats: a text of at least
//     duplicateMinRunes characters already sent to a different chat in the
//     last duplicateWindow is refused.
//  4. At most cfg.DailySendLimit messages a day (local calendar day). A
//     message to a group counts like any other. Reactions do not count.
//
// These come from WhatsApp's own ban patterns (first contact with strangers,
// one text to many chats, bursts), which matter more than the count.

const (
	duplicateMinRunes = 20
	duplicateWindow   = 30 * 24 * time.Hour
)

// sendPolicyMu holds the rules check and the flip to 'confirmed' together,
// so two confirms racing at 9 of 10 cannot both pass.
var sendPolicyMu sync.Mutex

type sendRefusal struct {
	Status         int    `json:"-"`
	Error          string `json:"error"`
	Details        string `json:"details"`
	SentToday      int    `json:"sent_today,omitempty"`
	DailyLimit     int    `json:"daily_limit,omitempty"`
	OpenInWhatsApp string `json:"open_in_whatsapp,omitempty"`
}

type pendingSend struct {
	RecipientJID string
	SendType     string
	Text         string
}

// checkSendPolicy returns nil when the send may go, or the refusal to return.
func checkSendPolicy(ctx context.Context, db *sql.DB, limit int, now time.Time, send pendingSend) (*sendRefusal, error) {
	if isBroadcastJID(send.RecipientJID) {
		return &sendRefusal{
			Status:  403,
			Error:   "broadcast_not_allowed",
			Details: "Mycelium never sends mass messages, so it does not send to broadcast lists, status or channels.",
		}, nil
	}
	if send.SendType == "reaction" {
		return nil, nil
	}
	aliases, err := resolveAliases(ctx, db, send.RecipientJID)
	if err != nil {
		return nil, err
	}

	var exists int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE chat_jid IN (`+inClausePlaceholders(len(aliases))+`) AND type != 'system' LIMIT 1)`,
		jidsToArgs(aliases)...,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("existing-conversation check: %w", err)
	}
	if exists == 0 {
		return &sendRefusal{
			Status:         403,
			Error:          "no_existing_conversation",
			Details:        "Mycelium sends only into conversations that already exist, never a first message to someone new. Send the first message yourself in WhatsApp.",
			OpenInWhatsApp: openInWhatsAppURL(aliases, send.Text),
		}, nil
	}

	sentToday, err := countSendsSince(ctx, db, localMidnight(now))
	if err != nil {
		return nil, err
	}
	if sentToday >= limit {
		return &sendRefusal{
			Status:         429,
			Error:          "daily_send_limit",
			Details:        fmt.Sprintf("Mycelium sends at most %d messages a day, and %d went out today. Copy this one and send it yourself in WhatsApp.", limit, sentToday),
			SentToday:      sentToday,
			DailyLimit:     limit,
			OpenInWhatsApp: openInWhatsAppURL(aliases, send.Text),
		}, nil
	}

	if norm := normalizeSendText(send.Text); len([]rune(norm)) >= duplicateMinRunes {
		dup, err := sentElsewhere(ctx, db, norm, aliases, now.Add(-duplicateWindow))
		if err != nil {
			return nil, err
		}
		if dup {
			return &sendRefusal{
				Status:  403,
				Error:   "same_text_to_several_chats",
				Details: "Mycelium never sends the same text to several chats, and this exact text already went to another chat. Change it for this person, or send it yourself in WhatsApp.",
			}, nil
		}
	}
	return nil, nil
}

// refuseSend writes the refusal and reports true when the rules stop this
// send. Only the rule's code is logged: the recipient and text stay out of
// the log.
func (s *Server) refuseSend(w http.ResponseWriter, r *http.Request, send pendingSend) bool {
	refusal, err := checkSendPolicy(r.Context(), s.db, s.cfg.DailySendLimit, time.Now(), send)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "send rules check failed", Details: err.Error()})
		return true
	}
	if refusal == nil {
		return false
	}
	log.Printf("send refused: %s", refusal.Error)
	writeJSON(w, refusal.Status, refusal)
	return true
}

// countSendsSince counts the messages that left (or are leaving) since t.
// A failed send did not reach anyone, so it does not use up the day.
func countSendsSince(ctx context.Context, db *sql.DB, t time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sends WHERE status IN ('confirmed', 'sent') AND send_type != 'reaction' AND confirmed_at >= ?`,
		t.Unix(),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("daily send count: %w", err)
	}
	return n, nil
}

func sentElsewhere(ctx context.Context, db *sql.DB, norm string, recipientAliases []string, since time.Time) (bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT recipient_jid, COALESCE(content_text, '') FROM sends
		 WHERE status IN ('confirmed', 'sent') AND send_type != 'reaction' AND confirmed_at >= ?`,
		since.Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("duplicate text check: %w", err)
	}
	defer rows.Close()
	same := map[string]bool{}
	for _, jid := range recipientAliases {
		same[jid] = true
	}
	for rows.Next() {
		var jid, text string
		if err := rows.Scan(&jid, &text); err != nil {
			return false, err
		}
		if !same[jid] && normalizeSendText(text) == norm {
			return true, nil
		}
	}
	return false, rows.Err()
}

// normalizeSendText compares texts the way a reader would: case and spacing
// do not make a message different.
func normalizeSendText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

func isBroadcastJID(jid string) bool {
	return strings.HasSuffix(jid, "@broadcast") || strings.HasSuffix(jid, "@newsletter")
}

// localMidnight is the start of the bridge's local calendar day, the day a
// member means by "10 a day".
func localMidnight(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
}

// openInWhatsAppURL is a wa.me link that opens the chat with the text filled
// in, for the member to send by hand. Only a phone-number JID has one;
// groups and LID-only contacts get none.
func openInWhatsAppURL(aliases []string, text string) string {
	for _, jid := range aliases {
		phone, ok := strings.CutSuffix(jid, "@s.whatsapp.net")
		phone, _, _ = strings.Cut(phone, ":") // drop a device part
		if !ok || phone == "" {
			continue
		}
		link := "https://wa.me/" + phone
		if text != "" {
			// %20, not QueryEscape's "+": wa.me fills the text in literally.
			link += "?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20")
		}
		return link
	}
	return ""
}
