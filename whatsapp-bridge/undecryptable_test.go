package main

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// MYC-3569 — a message that fails to DECRYPT must still produce a row.
//
// These tests drive b.handleEvent, NOT b.onUndecryptableMessage, and that is
// the whole point. The bug was never in the handler — there was no handler. The
// event fell through handleEvent's type switch and the bridge wrote nothing. A
// test that called the handler directly would pass against the broken build,
// which is the "green test proves nothing" trap. Going through the dispatcher
// makes the missing case a FAILURE, so these tests are their own negative
// control. Verified by deleting the case and re-running: see the PR body.

const (
	undecTestGroupJID = "12147735814-1589465137@g.us"
	undecTestSender   = "31628239888478:3@lid"
)

func undecryptableTestBridge(t *testing.T) (*Bridge, *sql.DB) {
	t.Helper()
	db := undecodedTestDB(t)
	return &Bridge{db: db}, db
}

func undecTestInfo(t *testing.T, id string, ts int64) types.MessageInfo {
	t.Helper()
	chat, err := types.ParseJID(undecTestGroupJID)
	if err != nil {
		t.Fatalf("parse chat jid: %v", err)
	}
	sender, err := types.ParseJID(undecTestSender)
	if err != nil {
		t.Fatalf("parse sender jid: %v", err)
	}
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:    chat,
			Sender:  sender,
			IsGroup: true,
		},
		ID:        id,
		PushName:  "Martha",
		Timestamp: time.Unix(ts, 0),
	}
}

func undecRow(t *testing.T, db *sql.DB, id string) (msgType, content string) {
	t.Helper()
	var ct sql.NullString
	err := db.QueryRow(`SELECT type, content_text FROM messages WHERE id = ?`, id).Scan(&msgType, &ct)
	if err != nil {
		t.Fatalf("read message %s: %v", id, err)
	}
	return msgType, ct.String
}

// The marker is the contract between the writer and every reader (export,
// healthcheck, onMessage's upgrade predicate). A round-trip test is what stops
// the halves from drifting apart silently.
func TestUndecryptableMarkerRoundTrip(t *testing.T) {
	if got := undecryptableFailMode(undecryptableMarker("unavailable")); got != "unavailable" {
		t.Fatalf("round-trip: want \"unavailable\", got %q", got)
	}
	for _, notAMarker := range []string{
		"", "hello",
		"[undecryptable: unterminated",
		"undecryptable: x]",
		// An [unsupported: ...] row is a DIFFERENT failure and must not be
		// mistaken for this one; the two counters have to stay separable.
		unsupportedMarker("eventMessage"),
	} {
		if got := undecryptableFailMode(notAMarker); got != "" {
			t.Fatalf("non-marker %q must yield \"\", got %q", notAMarker, got)
		}
	}
}

// The mode is derived only from fields the event actually carries. whatsmeow's
// log says "no sender key"; the event does not, so the bridge must not claim it.
func TestDecryptFailureModeNamesWhatTheEventCarries(t *testing.T) {
	cases := []struct {
		name string
		evt  *events.UndecryptableMessage
		want string
	}{
		{"plain decrypt failure", &events.UndecryptableMessage{}, "decrypt-failed"},
		{"unavailable", &events.UndecryptableMessage{IsUnavailable: true}, "unavailable"},
		{
			"unavailable with a type",
			&events.UndecryptableMessage{IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce},
			"unavailable:view_once",
		},
		{
			"hide mode is recorded, not dropped",
			&events.UndecryptableMessage{DecryptFailMode: events.DecryptFailHide},
			"decrypt-failed:hide",
		},
		{"nil", nil, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decryptFailureMode(tc.evt); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// A server-supplied token flows into a marker that readers parse back. An
// unsanitized "]" would truncate the marker and make the row unrecoverable —
// the same silent-loss class this ticket closes.
func TestUndecryptableMarkerSurvivesHostileUnavailableType(t *testing.T) {
	evt := &events.UndecryptableMessage{
		IsUnavailable:   true,
		UnavailableType: events.UnavailableType("evil] injected\nnewline"),
	}
	mode := decryptFailureMode(evt)
	marker := undecryptableMarker(mode)
	if got := undecryptableFailMode(marker); got != mode {
		t.Fatalf("hostile type broke the round-trip: stored %q, read back %q", marker, got)
	}
	if strings.ContainsAny(marker[len(undecryptablePrefix):len(marker)-1], "]\n") {
		t.Fatalf("marker body must not contain a delimiter or newline: %q", marker)
	}
}

// Item 1+2 — the event produces a persisted row. Driven through handleEvent, so
// an unhandled event fails this test (the negative control).
func TestUndecryptableMessagePersistsRow(t *testing.T) {
	b, db := undecryptableTestBridge(t)

	b.handleEvent(&events.UndecryptableMessage{
		Info:          undecTestInfo(t, "U1", 1784846700),
		IsUnavailable: true,
	})

	msgType, content := undecRow(t, db, "U1")
	if msgType != "system" {
		t.Fatalf("type: want \"system\" (the CHECK-allowed value), got %q", msgType)
	}
	if content != undecryptableMarker("unavailable") {
		t.Fatalf("content_text: want the undecryptable marker, got %q", content)
	}

	// The row must carry enough of evt.Info to be attributable, or the export
	// renders an orphan line.
	var sender, display string
	var ts int64
	if err := db.QueryRow(
		`SELECT sender_jid, sender_display, timestamp FROM messages WHERE id = ?`, "U1",
	).Scan(&sender, &display, &ts); err != nil {
		t.Fatalf("read row metadata: %v", err)
	}
	if sender != undecTestSender || display != "Martha" || ts != 1784846700 {
		t.Fatalf("row metadata lost: sender=%q display=%q ts=%d", sender, display, ts)
	}

	// The chat row must exist too — messages.chat_jid is a FOREIGN KEY, and a
	// chat we have never seen is exactly the case that would otherwise be
	// rejected and silently dropped.
	var chatCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid = ?`, undecTestGroupJID).Scan(&chatCount); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if chatCount != 1 {
		t.Fatalf("chat row: want 1, got %d", chatCount)
	}
}

// Item 6 — the load-bearing half. whatsmeow asks the sender for a resend, and a
// successful retry arrives as a normal events.Message on the SAME id. It must
// REPLACE the placeholder, in place, with no duplicate row.
func TestUndecryptableRetryUpgradesRowInPlace(t *testing.T) {
	b, db := undecryptableTestBridge(t)
	info := undecTestInfo(t, "U2", 1784846800)

	b.handleEvent(&events.UndecryptableMessage{Info: info, IsUnavailable: true})
	if _, content := undecRow(t, db, "U2"); content != undecryptableMarker("unavailable") {
		t.Fatalf("precondition: placeholder not stored, got %q", content)
	}

	// The retry lands.
	b.handleEvent(&events.Message{
		Info:    info,
		Message: &waE2E.Message{Conversation: proto.String("the message that finally decrypted")},
	})

	msgType, content := undecRow(t, db, "U2")
	if content != "the message that finally decrypted" {
		t.Fatalf("retry did not upgrade the placeholder in place; content_text is %q", content)
	}
	if msgType != "text" {
		t.Fatalf("retry must correct the type too: want \"text\", got %q", msgType)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, "U2").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("upgrade must not duplicate: want 1 row, got %d", n)
	}
}

// The upgrade is scoped. A real message must never be rewritten by a later
// event — that would be a regression of the DO NOTHING semantics every other
// message has always relied on.
func TestUpgradeNeverRewritesARealRow(t *testing.T) {
	b, db := undecryptableTestBridge(t)
	info := undecTestInfo(t, "U3", 1784846900)

	b.handleEvent(&events.Message{
		Info:    info,
		Message: &waE2E.Message{Conversation: proto.String("the original text")},
	})
	// A duplicate delivery of the same id with different content.
	b.handleEvent(&events.Message{
		Info:    info,
		Message: &waE2E.Message{Conversation: proto.String("a redelivery that must be ignored")},
	})

	if _, content := undecRow(t, db, "U3"); content != "the original text" {
		t.Fatalf("a real row was rewritten by a redelivery; content_text is %q", content)
	}
}

// A placeholder must never be DOWNGRADED. If the retry decodes to a genuinely
// content-free system row, keeping the loud marker is strictly better than
// replacing it with a blank — that blank is the MYC-3284 silent-empty-row bug.
func TestPlaceholderIsNotDowngradedByAnEmptyRetry(t *testing.T) {
	b, db := undecryptableTestBridge(t)
	info := undecTestInfo(t, "U4", 1784847000)

	b.handleEvent(&events.UndecryptableMessage{Info: info, IsUnavailable: true})
	// A retry carrying nothing the decoder can name: extractContent returns
	// ("", "system") for a message with no populated content field.
	b.handleEvent(&events.Message{Info: info, Message: &waE2E.Message{}})

	_, content := undecRow(t, db, "U4")
	if content != undecryptableMarker("unavailable") {
		t.Fatalf("placeholder was downgraded to %q — the marker must survive an empty retry", content)
	}
}

// Redelivery of the same undecryptable id must not duplicate or churn the row.
func TestRepeatedUndecryptableEventIsIdempotent(t *testing.T) {
	b, db := undecryptableTestBridge(t)
	info := undecTestInfo(t, "U5", 1784847100)

	b.handleEvent(&events.UndecryptableMessage{Info: info, IsUnavailable: true})
	b.handleEvent(&events.UndecryptableMessage{Info: info, IsUnavailable: true})

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, "U5").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row, got %d", n)
	}
}

// Item 4 — /healthcheck reports the rate, so it is measurable rather than found
// in a log tail. Counted separately from the MYC-3284 numbers, which must not
// move.
func TestUndecryptableStatsCountsByMode(t *testing.T) {
	db := undecodedTestDB(t)
	const jid = undecTestGroupJID
	insertTestMessage(t, db, "C1", jid, "text", "a normal message", 1784846600)
	insertTestMessage(t, db, "C2", jid, "system", undecryptableMarker("unavailable"), 1784846601)
	insertTestMessage(t, db, "C3", jid, "system", undecryptableMarker("unavailable"), 1784846602)
	insertTestMessage(t, db, "C4", jid, "system", undecryptableMarker("decrypt-failed"), 1784846603)
	// A malformed marker still counts — reported, never dropped.
	insertTestMessage(t, db, "C5", jid, "system", undecryptablePrefix+"truncated", 1784846604)
	// The MYC-3284 population must stay in its own bucket.
	insertTestMessage(t, db, "C6", jid, "system", unsupportedMarker("eventMessage"), 1784846605)
	insertTestMessage(t, db, "C7", jid, "system", "", 1784846606)

	s := &Server{db: db}
	st := s.undecodedStats(context.Background())

	if st.UndecryptableTotal != 4 {
		t.Fatalf("undecryptable_total: want 4, got %d", st.UndecryptableTotal)
	}
	if st.UndecryptableByMode["unavailable"] != 2 {
		t.Fatalf("undecryptable_by_mode[unavailable]: want 2, got %v", st.UndecryptableByMode)
	}
	if st.UndecryptableByMode["decrypt-failed"] != 1 {
		t.Fatalf("undecryptable_by_mode[decrypt-failed]: want 1, got %v", st.UndecryptableByMode)
	}
	if st.UndecryptableByMode["unknown"] != 1 {
		t.Fatalf("a malformed marker must count as \"unknown\", got %v", st.UndecryptableByMode)
	}
	// Cross-contamination check: the two failure classes are separate numbers.
	if st.UndecodedTotal != 1 {
		t.Fatalf("undecoded_total must not absorb undecryptable rows: want 1, got %d", st.UndecodedTotal)
	}
	if st.UndecodedByType["eventMessage"] != 1 {
		t.Fatalf("undecoded_by_type regressed: %v", st.UndecodedByType)
	}
	if st.LegacyEmptySystem != 1 {
		t.Fatalf("legacy_empty_system must not absorb undecryptable rows: want 1, got %d", st.LegacyEmptySystem)
	}
}

// Item 5 — the export renders an explicit placeholder. The negative control is
// the `if text == "" { continue }` path: the message must never be silently
// OMITTED from the chat file.
func TestExportRendersUndecryptablePlaceholder(t *testing.T) {
	db := undecodedTestDB(t)
	const jid = undecTestGroupJID
	insertTestMessage(t, db, "D1", jid, "text", "hello from the group", 1784846600)
	insertTestMessage(t, db, "D2", jid, "system", undecryptableMarker("unavailable"), 1784846683)

	outDir := t.TempDir()
	unit := exportUnit{
		members:       []string{jid},
		primary:       jid,
		chatType:      "group",
		display:       "Family",
		lastMessageTs: 1784846683,
		filename:      "Family (group).md",
	}
	if _, err := exportOneUnit(db, outDir, unit, "2026-07-24"); err != nil {
		t.Fatalf("exportOneUnit: %v", err)
	}

	var body string
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		body += string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if !strings.Contains(body, "hello from the group") {
		t.Fatalf("regressed the normal text path; export was:\n%s", body)
	}
	// Attributed to its sender, like [Voice note] — never an orphan line.
	if !strings.Contains(body, "Martha: [Undecryptable message: unavailable]") {
		t.Fatalf("MYC-3569: undecryptable message missing or unlabelled in the export:\n%s", body)
	}
}
