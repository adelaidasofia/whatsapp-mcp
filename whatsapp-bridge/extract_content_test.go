package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

// MYC-3284 — the negative control. An undecodable message type that CARRIES a
// content field must fail LOUD (a distinct, queryable "unsupported" row naming
// the real proto type), never silently collapse to an empty "system" row that
// reads as "no text". This test fails if the catch-all regresses to the drop.
func TestExtractContent_UndecodableTypeFailsLoud(t *testing.T) {
	// senderKeyDistributionMessage is a real whatsmeow type extractContent does
	// not decode — it stands in for any unhandled type (poll, event, view-once,
	// protocol, …) the switch does not name.
	evt := &events.Message{
		Message: &waE2E.Message{
			SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
		},
	}
	text, msgType := extractContent(evt)

	if msgType == "system" && text == "" {
		t.Fatal("MYC-3284 regression: undecodable message silently dropped as an empty \"system\" row")
	}
	if msgType != "unsupported" {
		t.Fatalf("undecodable message: want type \"unsupported\", got %q", msgType)
	}
	if !strings.HasPrefix(text, "[unsupported: ") {
		t.Fatalf("undecodable message: want a visible placeholder, got content %q", text)
	}
	if !strings.Contains(text, "senderKeyDistributionMessage") {
		t.Fatalf("unsupported row must name the raw proto type; got content %q", text)
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
	if msgType == "system" && text == "" {
		t.Fatal("MYC-3284 regression: undecodable baileys message silently imported as empty \"system\"")
	}
	if msgType != "unsupported" {
		t.Fatalf("want type \"unsupported\", got %q", msgType)
	}
	if !strings.Contains(text, "eventMessage") {
		t.Fatalf("unsupported row must name the raw type; got %q", text)
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
