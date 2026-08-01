package main

import (
	"context"
	"database/sql"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// The backfill rewrites rows in a live message store, so its eligibility
// predicate is the thing under test here — not the decoding, which is covered
// in content_decode_test.go. Every case below is a row the backfill must
// REFUSE to touch, plus the one shape it must repair.

func backfillDB(t *testing.T) (*Bridge, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", t.TempDir()+"/bf.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES ('c@g.us','group',0,0)`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	return &Bridge{db: db}, db
}

func seedRow(t *testing.T, db *sql.DB, id, typ, content string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO messages (id, chat_jid, timestamp, type, content_text) VALUES (?,?,?,?,?)`,
		id, "c@g.us", 1700000000, typ, content,
	); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func rowOf(t *testing.T, db *sql.DB, id string) (typ, content string) {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRow(`SELECT type, content_text FROM messages WHERE id=?`, id).Scan(&typ, &c); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return typ, c.String
}

// TestBackfillRepairsEmptySystemRow is the happy path: an envelope-wrapped text
// message stored empty by the old decoder is recovered.
func TestBackfillRepairsEmptySystemRow(t *testing.T) {
	b, db := backfillDB(t)
	seedRow(t, db, "m1", "system", "")

	const body = "text that was lost inside a disappearing-message envelope"
	n, err := b.backfillDecodedContent("m1", &waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{Conversation: proto.String(body)},
		},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
	typ, content := rowOf(t, db, "m1")
	if content != body {
		t.Errorf("content = %q, want %q", content, body)
	}
	if typ != "text" {
		t.Errorf("type = %q, want \"text\"", typ)
	}
}

// TestBackfillNeverOverwritesExistingContent is the one that matters most: this
// runs against a real store with ~113k rows, and a predicate that matched a row
// holding good text would destroy it. There is no undo.
func TestBackfillNeverOverwritesExistingContent(t *testing.T) {
	b, db := backfillDB(t)

	cases := []struct{ id, typ, content string }{
		{"keep-text", "text", "a real message that must survive"},
		{"keep-image", "image", "a caption"},
		{"keep-marker", "system", "[unsupported: encReactionMessage]"},
		{"keep-voice", "voice", ""}, // media legitimately has empty content
		{"keep-sticker", "sticker", ""},
	}
	for _, c := range cases {
		seedRow(t, db, c.id, c.typ, c.content)
	}

	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			n, err := b.backfillDecodedContent(c.id, &waE2E.Message{
				Conversation: proto.String("REPLACEMENT THAT MUST NOT LAND"),
			})
			if err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if n != 0 {
				t.Errorf("rows affected = %d, want 0 — the backfill claimed a row it must not touch", n)
			}
			typ, content := rowOf(t, db, c.id)
			if typ != c.typ || content != c.content {
				t.Errorf("row mutated: (%q,%q) -> (%q,%q)", c.typ, c.content, typ, content)
			}
		})
	}
}

// TestBackfillLeavesGenuineProtocolRowsAlone: a key-distribution carrier
// decodes to empty, and the row must be left exactly as it is rather than
// rewritten with identical emptiness or given a placeholder.
func TestBackfillLeavesGenuineProtocolRowsAlone(t *testing.T) {
	b, db := backfillDB(t)
	seedRow(t, db, "carrier", "system", "")

	n, err := b.backfillDecodedContent("carrier", &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d, want 0 — a textless carrier needs no rewrite", n)
	}
	typ, content := rowOf(t, db, "carrier")
	if typ != "system" || content != "" {
		t.Errorf("carrier row changed to (%q,%q)", typ, content)
	}
}

// TestBackfillIsIdempotent: overlapping history chunks re-deliver the same
// message, and the second pass must be a no-op rather than a second write.
func TestBackfillIsIdempotent(t *testing.T) {
	b, db := backfillDB(t)
	seedRow(t, db, "m1", "system", "")

	msg := &waE2E.Message{Conversation: proto.String("recovered once")}
	first, _ := b.backfillDecodedContent("m1", msg)
	second, _ := b.backfillDecodedContent("m1", msg)

	if first != 1 {
		t.Fatalf("first pass affected %d rows, want 1", first)
	}
	if second != 0 {
		t.Errorf("second pass affected %d rows, want 0 — repeated chunks must not rewrite", second)
	}
}

// TestBackfillHandlesNilAndEmptyInputs guards the loop that calls this for
// every message in every history chunk.
func TestBackfillHandlesNilAndEmptyInputs(t *testing.T) {
	b, _ := backfillDB(t)
	for _, tc := range []struct {
		name string
		id   string
		msg  *waE2E.Message
	}{
		{"nil message", "x", nil},
		{"empty id", "", &waE2E.Message{Conversation: proto.String("hi")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := b.backfillDecodedContent(tc.id, tc.msg)
			if err != nil || n != 0 {
				t.Errorf("got (%d, %v), want (0, nil)", n, err)
			}
		})
	}
}

// TestCountEmptySystemRowsMeasuresTheRightShape backs the before/after number
// the ticket asks for, so the reported progress means what it claims.
func TestCountEmptySystemRowsMeasuresTheRightShape(t *testing.T) {
	b, db := backfillDB(t)
	seedRow(t, db, "e1", "system", "")
	seedRow(t, db, "e2", "system", "")
	seedRow(t, db, "marked", "system", "[unsupported: pollUpdateMessage]")
	seedRow(t, db, "real", "text", "hello")

	n, err := b.CountEmptySystemRows(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 — a marker row is NOT an empty row and must not inflate the backlog", n)
	}
}
