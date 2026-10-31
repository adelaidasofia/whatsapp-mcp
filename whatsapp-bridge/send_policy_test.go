package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	policyPhone = "15555550100@s.whatsapp.net"
	policyLID   = "77777777777777@lid"
	policyOther = "15555550199@s.whatsapp.net"
	policyGroup = "120363000000000001@g.us"
)

func insertPolicyMessage(t *testing.T, db *sql.DB, id, chat, kind string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO messages (id, chat_jid, timestamp, type, content_text) VALUES (?, ?, 100, ?, 'hola')`,
		id, chat, kind,
	); err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

func insertPolicySend(t *testing.T, db *sql.DB, id, recipient, sendType, text, status string, confirmedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO sends (draft_id, recipient_jid, send_type, content_text, status, created_at, confirmed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, recipient, sendType, text, status, confirmedAt.Unix(), confirmedAt.Unix(),
	); err != nil {
		t.Fatalf("insert send %s: %v", id, err)
	}
}

func policyCheck(t *testing.T, db *sql.DB, limit int, now time.Time, send pendingSend) *sendRefusal {
	t.Helper()
	refusal, err := checkSendPolicy(t.Context(), db, limit, now, send)
	if err != nil {
		t.Fatalf("checkSendPolicy: %v", err)
	}
	return refusal
}

// noon keeps "today" and "yesterday" unambiguous in any local time zone.
func noon() time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}

func TestSendPolicyNeverSendsToBroadcastsStatusOrChannels(t *testing.T) {
	db := newSendsTestDB(t)
	for _, jid := range []string{"status@broadcast", "1234567890@broadcast", "120363000000000002@newsletter"} {
		insertPolicyMessage(t, db, "m-"+jid, jid, "text")
		for _, kind := range []string{"text", "reaction"} {
			got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: jid, SendType: kind, Text: "hola"})
			if got == nil || got.Error != "broadcast_not_allowed" {
				t.Errorf("%s (%s): got %+v, want broadcast_not_allowed", jid, kind, got)
			}
		}
	}
}

func TestSendPolicyOnlySendsIntoConversationsThatExist(t *testing.T) {
	db := newSendsTestDB(t)
	got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola qué tal"})
	if got == nil || got.Error != "no_existing_conversation" || got.Status != 403 {
		t.Fatalf("first contact: got %+v, want 403 no_existing_conversation", got)
	}
	if got.OpenInWhatsApp != "https://wa.me/15555550100?text=hola%20qu%C3%A9%20tal" {
		t.Errorf("open_in_whatsapp = %q", got.OpenInWhatsApp)
	}

	// A system row alone (an "encryption changed" notice) is not a conversation.
	insertPolicyMessage(t, db, "sys", policyPhone, "system")
	if got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola"}); got == nil {
		t.Fatal("a chat with only a system message was treated as an existing conversation")
	}

	// The conversation is stored under the person's LID; the draft names
	// their phone JID. The alias makes it the same conversation.
	insertPolicyMessage(t, db, "real", policyLID, "text")
	if _, err := db.Exec(`INSERT INTO jid_aliases (jid_a, jid_b, discovered_at, source) VALUES (?, ?, 1, 'test')`, policyPhone, policyLID); err != nil {
		t.Fatal(err)
	}
	if got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola"}); got != nil {
		t.Fatalf("existing conversation through an alias was refused: %+v", got)
	}
}

func TestSendPolicyCapsMessagesPerDayAndGroupsCount(t *testing.T) {
	db := newSendsTestDB(t)
	insertPolicyMessage(t, db, "m1", policyPhone, "text")
	insertPolicyMessage(t, db, "m2", policyGroup, "text")
	now := noon()
	yesterday := now.Add(-24 * time.Hour)

	// What does not use up today: yesterday's sends, failed sends, reactions,
	// drafts never confirmed.
	for i := range 10 {
		insertPolicySend(t, db, fmt.Sprintf("y%d", i), policyPhone, "text", fmt.Sprintf("y %d", i), "sent", yesterday)
	}
	insertPolicySend(t, db, "failed", policyPhone, "text", "f", "failed", now)
	insertPolicySend(t, db, "reaction", policyPhone, "reaction", "👍", "sent", now)
	insertPolicySend(t, db, "draft", policyPhone, "text", "d", "draft", now)

	// Nine today, three of them to a group.
	for i := range 9 {
		to := policyPhone
		if i < 3 {
			to = policyGroup
		}
		insertPolicySend(t, db, fmt.Sprintf("t%d", i), to, "text", fmt.Sprintf("today %d", i), "sent", now.Add(-time.Duration(i)*time.Minute))
	}
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyGroup, SendType: "text", Text: "the tenth"}); got != nil {
		t.Fatalf("the tenth send of the day was refused: %+v", got)
	}

	insertPolicySend(t, db, "t9", policyGroup, "text", "the tenth", "confirmed", now)
	got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "the eleventh"})
	if got == nil || got.Error != "daily_send_limit" || got.Status != 429 || got.SentToday != 10 || got.DailyLimit != 10 {
		t.Fatalf("the eleventh send: got %+v, want 429 daily_send_limit at 10 of 10", got)
	}
	if !strings.HasPrefix(got.OpenInWhatsApp, "https://wa.me/15555550100?text=") {
		t.Errorf("open_in_whatsapp = %q, want the wa.me link to copy it by hand", got.OpenInWhatsApp)
	}
	// A reaction is not a message and still goes.
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyPhone, SendType: "reaction", Text: "👍"}); got != nil {
		t.Errorf("a reaction at the limit was refused: %+v", got)
	}
}

func TestSendPolicyLimitZeroTurnsSendingOff(t *testing.T) {
	db := newSendsTestDB(t)
	insertPolicyMessage(t, db, "m1", policyPhone, "text")
	for _, kind := range []string{"text", "reaction"} {
		if got := policyCheck(t, db, 0, noon(), pendingSend{RecipientJID: policyPhone, SendType: kind, Text: "hola"}); got == nil || got.Error != "daily_send_limit" {
			t.Fatalf("limit 0, %s: got %+v, want daily_send_limit", kind, got)
		}
	}
}

// A reaction to a stranger must not open the door to a first message: the
// stored reaction row is not a conversation, and the reaction itself needs one.
func TestSendPolicyAReactionCannotOpenAConversation(t *testing.T) {
	db := newSendsTestDB(t)
	if got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "reaction", Text: "👍"}); got == nil || got.Error != "no_existing_conversation" {
		t.Fatalf("a reaction into no conversation: got %+v, want no_existing_conversation", got)
	}
	insertPolicyMessage(t, db, "my-reaction", policyPhone, "reaction")
	if got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola"}); got == nil || got.Error != "no_existing_conversation" {
		t.Fatalf("a stored reaction counted as a conversation: got %+v", got)
	}
}

// Writing to yourself ("message yourself", owner notifications) is not
// messaging anyone: no conversation needed, and it never uses up the day.
func TestSendPolicyMessagesToYourselfAreExempt(t *testing.T) {
	db := newSendsTestDB(t)
	own := []string{"15555550142@s.whatsapp.net", "88888888888888@lid"}
	insertPolicyMessage(t, db, "m1", policyPhone, "text")
	now := noon()
	for i := range 12 {
		insertPolicySend(t, db, fmt.Sprintf("self%d", i), own[0], "text", fmt.Sprintf("nota %d", i), "sent", now)
	}
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: own[1], SendType: "text", Text: "otra nota", Own: own}); got != nil {
		t.Fatalf("a note to yourself was refused: %+v", got)
	}
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola", Own: own}); got != nil {
		t.Fatalf("twelve notes to yourself used up the day: %+v", got)
	}
}

// whatsmeow's own LID store can know a person's other JID before
// jid_aliases does (that table is backfilled at startup).
func TestSendPolicyUsesTheLIDStoreAlias(t *testing.T) {
	db := newSendsTestDB(t)
	insertPolicyMessage(t, db, "m1", policyLID, "text")
	if got := policyCheck(t, db, 10, noon(), pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "hola", Alts: []string{policyLID}}); got != nil {
		t.Fatalf("a chat known only through the LID store was refused: %+v", got)
	}
}

func TestSendPolicyNeverSendsTheSameTextToSeveralChats(t *testing.T) {
	db := newSendsTestDB(t)
	insertPolicyMessage(t, db, "m1", policyPhone, "text")
	insertPolicyMessage(t, db, "m2", policyOther, "text")
	now := noon()
	insertPolicySend(t, db, "s1", policyPhone, "text", "Te comparto la propuesta para la reunión de mañana", "sent", now.Add(-2*time.Hour))

	// Same words, different case and spacing, to a different chat.
	got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyOther, SendType: "text", Text: "te comparto  la propuesta para la reunión de MAÑANA"})
	if got == nil || got.Error != "same_text_to_several_chats" {
		t.Fatalf("same text to a second chat: got %+v, want same_text_to_several_chats", got)
	}
	if !strings.HasPrefix(got.OpenInWhatsApp, "https://wa.me/15555550199?text=") {
		t.Errorf("open_in_whatsapp = %q, want the link to send it by hand", got.OpenInWhatsApp)
	}
	// An invisible character does not make it a different message.
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyOther, SendType: "text", Text: "Te comparto la propuesta para la reunión de mañana​"}); got == nil {
		t.Error("a trailing zero-width space got the same text through")
	}
	// The same chat again is not "several chats".
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyPhone, SendType: "text", Text: "Te comparto la propuesta para la reunión de mañana"}); got != nil {
		t.Errorf("the same text to the same chat was refused: %+v", got)
	}
	// A short courtesy text is not a broadcast.
	insertPolicySend(t, db, "s2", policyPhone, "text", "Muchas gracias", "sent", now.Add(-time.Hour))
	if got := policyCheck(t, db, 10, now, pendingSend{RecipientJID: policyOther, SendType: "text", Text: "muchas gracias"}); got != nil {
		t.Errorf("a short courtesy text was refused: %+v", got)
	}
	// A day later it is not a burst any more.
	db2 := newSendsTestDB(t)
	insertPolicyMessage(t, db2, "m2", policyOther, "text")
	insertPolicySend(t, db2, "old", policyPhone, "text", "Te comparto la propuesta para la reunión de mañana", "sent", now.Add(-25*time.Hour))
	if got := policyCheck(t, db2, 10, now, pendingSend{RecipientJID: policyOther, SendType: "text", Text: "Te comparto la propuesta para la reunión de mañana"}); got != nil {
		t.Errorf("a text sent 25 hours ago blocked a new send: %+v", got)
	}
}

func TestLocalMidnightStartsTheMembersDay(t *testing.T) {
	bogota := time.FixedZone("COT", -5*3600)
	at := time.Date(2026, 10, 31, 0, 30, 0, 0, bogota)
	if got := localMidnight(at); !got.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, bogota)) {
		t.Errorf("localMidnight(%v) = %v", at, got)
	}
}
