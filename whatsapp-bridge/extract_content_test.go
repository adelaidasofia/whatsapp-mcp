package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

// MYC-3284 — the negative control. An undecodable message that CARRIES a
// content field must fail LOUD (a non-empty marker naming the real proto type),
// never silently collapse to an empty row that reads as "no text". This test
// fails if the catch-all regresses to the silent drop.
func TestExtractContent_UndecodableTypeFailsLoud(t *testing.T) {
	// pollCreationMessage is a real whatsmeow type extractContent does not
	// decode AND which genuinely carries user content — it stands in for any
	// unhandled content type (poll, event, view-once, edited, …).
	evt := &events.Message{
		Message: &waE2E.Message{
			PollCreationMessage: &waE2E.PollCreationMessage{},
		},
	}
	text, msgType := extractContent(evt)

	if text == "" {
		t.Fatalf("MYC-3284 regression: undecodable message silently dropped as an empty %q row", msgType)
	}
	if unsupportedRawType(text) != "pollCreationMessage" {
		t.Fatalf("undecodable message must carry a marker naming the raw proto type; got content %q", text)
	}
}

// A message carrying ONLY cryptographic/protocol plumbing has no user-visible
// text, so it must stay a plain empty "system" row — NOT a placeholder line in
// the member's chat file. Measured live: senderKeyDistributionMessage was 6 of
// the first 8 markers written to the vault. The fail-loud property is intact:
// see the test above, where an unknown CONTENT type still gets its marker.
func TestExtractContent_ProtocolCarrierStaysSilent(t *testing.T) {
	text, msgType := extractContent(&events.Message{
		Message: &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
			MessageContextInfo:           &waE2E.MessageContextInfo{},
		},
	})
	if text != "" || msgType != "system" {
		t.Fatalf("protocol-only message must stay a silent empty system row, got (%q, %q)", text, msgType)
	}
}

// ...but a carrier field riding ALONGSIDE an undecoded content field must not
// mask it: the content field is still named.
func TestExtractContent_CarrierDoesNotMaskRealContent(t *testing.T) {
	text, _ := extractContent(&events.Message{
		Message: &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
			EventMessage:                 &waE2E.EventMessage{},
		},
	})
	if unsupportedRawType(text) != "eventMessage" {
		t.Fatalf("carrier field must not mask a real undecoded content type; got %q", text)
	}
}

// The fail-loud default must not regress the decoded happy path.
func TestExtractContent_KnownTextStillDecodes(t *testing.T) {
	conv := "hello from the group"
	evt := &events.Message{Message: &waE2E.Message{Conversation: &conv}}
	text, msgType := extractContent(evt)
	if msgType != "text" || text != conv {
		t.Fatalf("regressed the text path: got (%q, %q), want (%q, \"text\")", text, msgType, conv)
	}
}

// A genuinely content-free message (no populated content field, including one
// that carries only messageContextInfo metadata) is truly textless and keeps
// its existing "system" classification — the fix is surgical, not reclassify-all.
func TestExtractContent_ContentFreeStaysSystem(t *testing.T) {
	cases := map[string]*waE2E.Message{
		"truly empty":   {},
		"metadata only": {MessageContextInfo: &waE2E.MessageContextInfo{}},
	}
	for name, msg := range cases {
		text, msgType := extractContent(&events.Message{Message: msg})
		if msgType != "system" || text != "" {
			t.Fatalf("%s: want (\"\", \"system\") unchanged, got (%q, %q)", name, text, msgType)
		}
	}
}

// MYC-3284 — the Baileys import path (baileysExtractContent) carries the same
// silent-drop class; an undecodable imported type must also fail loud.
func TestBaileysExtractContent_UndecodableTypeFailsLoud(t *testing.T) {
	text, msgType := baileysExtractContent(map[string]any{
		"messageContextInfo": map[string]any{},            // metadata — must be skipped
		"eventMessage":       map[string]any{"name": "x"}, // an unhandled content type
	})
	if text == "" {
		t.Fatalf("MYC-3284 regression: undecodable baileys message silently imported as an empty %q row", msgType)
	}
	if !strings.Contains(text, "eventMessage") {
		t.Fatalf("marker must name the raw type; got %q", text)
	}
}

func TestBaileysExtractContent_ContentFreeStaysSystem(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"empty":         {},
		"metadata only": {"messageContextInfo": map[string]any{}},
	} {
		text, msgType := baileysExtractContent(m)
		if msgType != "system" || text != "" {
			t.Fatalf("%s: want (\"\", \"system\"), got (%q, %q)", name, text, msgType)
		}
	}
}

// THE structural guard for this bug class (MYC-3284 follow-up). The first fix
// returned a NEW type string ("unsupported") that the messages.type CHECK
// constraint in migrations/001 does not admit, so every undecodable message
// failed its INSERT and was stored NOWHERE — a silent drop worse than the empty
// row it replaced, invisible to every unit test that never touched the schema.
//
// This asserts the contract that matters: whatever type extractContent returns
// must be STORABLE. It drives the real migrated schema, so a future edit that
// invents another type value fails here instead of in production.
func TestExtractContentTypesAreStorableUnderSchema(t *testing.T) {
	db := undecodedTestDB(t)
	conv := "hello"

	cases := map[string]*waE2E.Message{
		"undecodable":  {SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}},
		"decoded text": {Conversation: &conv},
		"content free": {},
	}
	for name, msg := range cases {
		text, msgType := extractContent(&events.Message{Message: msg})
		_, err := db.Exec(
			`INSERT INTO messages (id, chat_jid, sender_jid, timestamp, type, content_text, is_from_me)
			 VALUES (?, ?, ?, ?, ?, ?, 0)`,
			"id-"+name, "12147735814-1589465137@g.us", "31628239888478:3@lid", 1784846683, msgType, text)
		if err != nil {
			t.Fatalf("%s: extractContent returned type %q, which the schema rejects — the message would be stored NOWHERE: %v",
				name, msgType, err)
		}
	}
}
