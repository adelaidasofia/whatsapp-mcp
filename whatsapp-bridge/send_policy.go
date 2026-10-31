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
	"unicode"

	"go.mau.fi/whatsmeow/types"
)

// The rules every outbound message passes before it leaves. They live here,
// in the bridge, because its loopback API has no authentication: a limit in
// the MCP server or in an app that runs the bridge binds only the callers
// that go through them, while anything on the machine can POST to
// /api/sends. Drafting stays free; the rules bind at confirm.
//
//  1. Never to a broadcast list, status or channel.
//  2. Only into a conversation that already exists: the chat, or another JID
//     of the same person, has a stored message that someone wrote (not a
//     system notice, not a reaction). No first contact with strangers, and a
//     reaction cannot open a conversation either.
//  3. Never the same text to several chats: a text of at least
//     duplicateMinRunes characters already sent to a different chat within
//     duplicateWindow is refused. That is the burst pattern WhatsApp bans.
//  4. At most cfg.DailySendLimit messages a local calendar day. A message to
//     a group counts like any other. Reactions do not count, but a limit of 0
//     stops them too: 0 means no sending at all.
//
// Messages to the account's own chat ("message yourself") are exempt from
// 2 to 4: writing to yourself is not messaging anyone.

const (
	duplicateMinRunes = 20
	duplicateWindow   = 24 * time.Hour
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
	// Alts are other JIDs of the recipient that whatsmeow's own LID store
	// knows and jid_aliases may not have yet (it is backfilled at startup).
	Alts []string
	// Own are the account's own JIDs: its phone JID and its LID.
	Own []string
}

// checkSendPolicy returns nil when the send may go, or the refusal to return.
func checkSendPolicy(ctx context.Context, db *sql.DB, limit int, now time.Time, send pendingSend) (*sendRefusal, error) {
	if isBroadcastJID(send.RecipientJID) {
		return &sendRefusal{
			Status:  403,
			Error:   "broadcast_not_allowed",
			Details: "This WhatsApp bridge never sends mass messages, so it does not send to broadcast lists, status or channels.",
		}, nil
	}
	aliases, err := resolveAliases(ctx, db, send.RecipientJID)
	if err != nil {
		return nil, err
	}
	aliases = appendNew(aliases, send.Alts...)
	if overlaps(aliases, send.Own) {
		return nil, nil
	}
	link := openInWhatsAppURL(aliases, send.Text)

	if send.SendType == "reaction" && limit == 0 {
		return &sendRefusal{Status: 429, Error: "daily_send_limit", Details: "Sending is turned off on this WhatsApp bridge.", DailyLimit: limit}, nil
	}

	var exists int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE chat_jid IN (`+inClausePlaceholders(len(aliases))+`)
		 AND type NOT IN ('system', 'reaction') LIMIT 1)`,
		jidsToArgs(aliases)...,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("existing-conversation check: %w", err)
	}
	if exists == 0 {
		return &sendRefusal{
			Status:         403,
			Error:          "no_existing_conversation",
			Details:        "This WhatsApp bridge sends only into conversations that already exist, never a first message to someone new. Send the first message yourself in WhatsApp.",
			OpenInWhatsApp: link,
		}, nil
	}
	if send.SendType == "reaction" {
		return nil, nil
	}

	sentToday, err := countSendsSince(ctx, db, localMidnight(now), send.Own)
	if err != nil {
		return nil, err
	}
	if sentToday >= limit {
		return &sendRefusal{
			Status:         429,
			Error:          "daily_send_limit",
			Details:        fmt.Sprintf("This WhatsApp bridge sends at most %d messages a day, and %d went out today. Copy this one and send it yourself in WhatsApp.", limit, sentToday),
			SentToday:      sentToday,
			DailyLimit:     limit,
			OpenInWhatsApp: link,
		}, nil
	}

	if norm := normalizeSendText(send.Text); len([]rune(norm)) >= duplicateMinRunes {
		dup, err := sentElsewhere(ctx, db, norm, appendNew(aliases, send.Own...), now.Add(-duplicateWindow))
		if err != nil {
			return nil, err
		}
		if dup {
			return &sendRefusal{
				Status:         403,
				Error:          "same_text_to_several_chats",
				Details:        "This WhatsApp bridge never sends the same text to several chats, and this exact text went to another chat in the last day. Change it for this person, or send it yourself in WhatsApp.",
				OpenInWhatsApp: link,
			}, nil
		}
	}
	return nil, nil
}

// refuseSend writes the refusal and reports true when the rules stop this
// send. Only the rule's code is logged: the recipient and text stay out of
// the log.
func (s *Server) refuseSend(w http.ResponseWriter, r *http.Request, send pendingSend) bool {
	send.Own, send.Alts = s.ownJIDs(), s.altJIDs(r.Context(), send.RecipientJID)
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

// refuseSendLocked is refuseSend under sendPolicyMu, released even if the
// check panics. A refused send returns true; otherwise flip runs under the
// same lock.
func (s *Server) refuseSendLocked(w http.ResponseWriter, r *http.Request, send pendingSend, flip func()) bool {
	sendPolicyMu.Lock()
	defer sendPolicyMu.Unlock()
	if s.refuseSend(w, r, send) {
		return true
	}
	flip()
	return false
}

func (s *Server) ownJIDs() []string {
	if s.bridge == nil || s.bridge.client == nil || s.bridge.client.Store == nil {
		return nil
	}
	var own []string
	if id := s.bridge.client.Store.ID; id != nil {
		own = append(own, id.ToNonAD().String())
	}
	if lid := s.bridge.client.Store.LID; !lid.IsEmpty() {
		own = append(own, lid.ToNonAD().String())
	}
	return own
}

func (s *Server) altJIDs(ctx context.Context, jid string) []string {
	if s.bridge == nil || s.bridge.client == nil || s.bridge.client.Store == nil {
		return nil
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return nil
	}
	alt, err := s.bridge.client.Store.GetAltJID(ctx, parsed)
	if err != nil || alt.IsEmpty() {
		return nil
	}
	return []string{alt.ToNonAD().String()}
}

// countSendsSince counts the messages that left (or are leaving) since t,
// leaving out messages to the account's own chat. A failed send did not
// reach anyone, so it does not use up the day.
func countSendsSince(ctx context.Context, db *sql.DB, t time.Time, own []string) (int, error) {
	query := `SELECT COUNT(*) FROM sends WHERE status IN ('confirmed', 'sent') AND send_type != 'reaction' AND confirmed_at >= ?`
	args := []any{t.Unix()}
	if len(own) > 0 {
		query += ` AND recipient_jid NOT IN (` + inClausePlaceholders(len(own)) + `)`
		args = append(args, jidsToArgs(own)...)
	}
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("daily send count: %w", err)
	}
	return n, nil
}

// sentElsewhere reports whether `norm` already went to a chat outside
// `same` (the recipient's JIDs and the account's own) since `since`.
func sentElsewhere(ctx context.Context, db *sql.DB, norm string, same []string, since time.Time) (bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT recipient_jid, COALESCE(content_text, '') FROM sends
		 WHERE status IN ('confirmed', 'sent') AND send_type != 'reaction' AND confirmed_at >= ?`,
		since.Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("duplicate text check: %w", err)
	}
	defer rows.Close()
	skip := map[string]bool{}
	for _, jid := range same {
		skip[jid] = true
	}
	for rows.Next() {
		var jid, text string
		if err := rows.Scan(&jid, &text); err != nil {
			return false, err
		}
		if !skip[jid] && normalizeSendText(text) == norm {
			return true, nil
		}
	}
	return false, rows.Err()
}

// normalizeSendText compares texts the way a reader would: case, spacing and
// invisible characters (a zero-width space, a direction mark) do not make a
// message different.
func normalizeSendText(text string) string {
	visible := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(strings.ToLower(visible)), " ")
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

func appendNew(list []string, more ...string) []string {
	seen := map[string]bool{}
	for _, item := range list {
		seen[item] = true
	}
	for _, item := range more {
		if item != "" && !seen[item] {
			seen[item] = true
			list = append(list, item)
		}
	}
	return list
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
