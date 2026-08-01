package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func msgEvent(m *waE2E.Message) *events.Message { return &events.Message{Message: m} }

func convo(s string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(s)}
}

// --- envelopes -------------------------------------------------------------

// TestEnvelopeWrappedTextIsDecodedNotMarked is the regression this change
// exists for. Before unwrapping, a text message inside any of these wrappers
// matched no case and was stored as "[unsupported: <wrapper>]" — visible, per
// MYC-3284, but with the text itself never captured.
func TestEnvelopeWrappedTextIsDecodedNotMarked(t *testing.T) {
	const body = "the message a human can plainly read in the client"

	cases := []struct {
		name string
		msg  *waE2E.Message
	}{
		{"ephemeral (disappearing messages)", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"view once", &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"view once v2", &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"view once v2 extension", &waE2E.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"document with caption", &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"device sent", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: convo(body)}}},
		{"edited", &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: convo(body)}}},
		{"protocol edit", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{EditedMessage: convo(body)}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, typ := extractContent(msgEvent(tc.msg))

			if unsupportedRawType(text) != "" {
				t.Fatalf("wrapped text was marked %q instead of decoded: the message is visible "+
					"but its text is still not captured", text)
			}
			if text != body {
				t.Errorf("text = %q, want %q", text, body)
			}
			if typ != "text" {
				t.Errorf("type = %q, want \"text\"", typ)
			}
		})
	}
}

// TestNestedEnvelopeUnwraps covers an edit of a disappearing message, the
// realistic two-deep case.
func TestNestedEnvelopeUnwraps(t *testing.T) {
	const body = "edited inside an ephemeral envelope"
	text, typ := extractContent(msgEvent(&waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				EditedMessage: &waE2E.FutureProofMessage{Message: convo(body)},
			},
		},
	}))
	if text != body || typ != "text" {
		t.Errorf("got (%q, %q), want (%q, \"text\")", text, typ, body)
	}
}

// TestEnvelopeWrappedMediaKeepsItsType: unwrapping must not flatten a wrapped
// image into a bare text row.
func TestEnvelopeWrappedMediaKeepsItsType(t *testing.T) {
	const caption = "caption inside a view-once envelope"
	text, typ := extractContent(msgEvent(&waE2E.Message{
		ViewOnceMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption)},
			},
		},
	}))
	if typ != "image" {
		t.Errorf("type = %q, want \"image\"", typ)
	}
	if text != caption {
		t.Errorf("text = %q, want %q", text, caption)
	}
}

// TestEmptyEnvelopeStillFailsLoud: a wrapper with nothing inside must not
// become a silent empty row just because unwrapping ran.
func TestEmptyEnvelopeStillFailsLoud(t *testing.T) {
	text, _ := extractContent(msgEvent(&waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{},
	}))
	if text == "" {
		t.Fatal("an empty envelope collapsed to a silent empty row — the MYC-3284 " +
			"fail-loud property must survive unwrapping")
	}
}

// --- second-tier decoders --------------------------------------------------

// TestPreviouslyMarkedTypesNowDecode covers the types observed as live markers
// on the bridge after MYC-3284 shipped. Each carries text a human reads in the
// chat, so a marker was the wrong answer for them.
func TestPreviouslyMarkedTypesNowDecode(t *testing.T) {
	cases := []struct {
		name     string
		msg      *waE2E.Message
		wantType string
		wantSubs []string
	}{
		{
			name: "poll v3 (seen live)",
			msg: &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
				Name: proto.String("Ship it?"),
				Options: []*waE2E.PollCreationMessage_Option{
					{OptionName: proto.String("Yes")}, {OptionName: proto.String("No")},
				},
			}},
			wantType: "text",
			wantSubs: []string{"Ship it?", "Yes", "No"},
		},
		{
			name:     "live location (seen live)",
			msg:      &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{Caption: proto.String("on my way")}},
			wantType: "location",
			wantSubs: []string{"on my way"},
		},
		{
			name: "contacts array (seen live)",
			msg: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
				DisplayName: proto.String("Team"),
				Contacts:    []*waE2E.ContactMessage{{DisplayName: proto.String("Ada")}},
			}},
			wantType: "contact",
			wantSubs: []string{"Team", "Ada"},
		},
		{
			name: "event",
			msg: &waE2E.Message{EventMessage: &waE2E.EventMessage{
				Name: proto.String("Launch review"), Description: proto.String("Go/no-go"),
			}},
			wantType: "text",
			wantSubs: []string{"Launch review", "Go/no-go"},
		},
		{
			name:     "group invite",
			msg:      &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Founders")}},
			wantType: "text",
			wantSubs: []string{"Founders"},
		},
		{
			name:     "video note",
			msg:      &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: proto.String("quick take")}},
			wantType: "video",
			wantSubs: []string{"quick take"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, typ := extractContent(msgEvent(tc.msg))
			if unsupportedRawType(text) != "" {
				t.Fatalf("still marked unsupported (%q); a decoder exists for this type now", text)
			}
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(text, sub) {
					t.Errorf("text %q missing %q", text, sub)
				}
			}
		})
	}
}

// TestDecodedTypesUseAllowedCheckValues guards the constraint the whole design
// leans on: these decoders deliberately map onto messages.type values already
// permitted by the CHECK in migrations/001, so no table rebuild is needed. A
// new type value here would be rejected at INSERT time in production while
// every unit test still passed.
func TestDecodedTypesUseAllowedCheckValues(t *testing.T) {
	allowed := map[string]bool{
		"text": true, "image": true, "video": true, "audio": true, "voice": true,
		"document": true, "sticker": true, "location": true, "contact": true,
		"call": true, "system": true, "reaction": true,
	}
	probes := []*waE2E.Message{
		{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		{PollCreationMessageV2: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		{PollCreationMessageV5: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		{PollCreationMessageV6: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		{EventMessage: &waE2E.EventMessage{Name: proto.String("e")}},
		{LiveLocationMessage: &waE2E.LiveLocationMessage{}},
		{ContactsArrayMessage: &waE2E.ContactsArrayMessage{}},
		{GroupInviteMessage: &waE2E.GroupInviteMessage{}},
		{PtvMessage: &waE2E.VideoMessage{}},
		{ProductMessage: &waE2E.ProductMessage{}},
		{OrderMessage: &waE2E.OrderMessage{}},
		{ListMessage: &waE2E.ListMessage{}},
		{ListResponseMessage: &waE2E.ListResponseMessage{}},
		{ButtonsMessage: &waE2E.ButtonsMessage{}},
		{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{}},
		{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{}},
		{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{}},
	}
	for _, p := range probes {
		_, typ, ok := decodeExtraContent(p)
		if !ok {
			t.Errorf("decodeExtraContent returned ok=false for %T probe", p)
			continue
		}
		if !allowed[typ] {
			t.Errorf("type %q is not in the messages.type CHECK allow-list; "+
				"production INSERTs would fail while unit tests pass", typ)
		}
	}
}

// --- fail-loud must survive ------------------------------------------------

// TestUnknownTypeStillFailsLoud is the negative control carried over from
// MYC-3284: a type NEITHER tier decodes must still keep its marker. This is the
// property most at risk from adding decode tiers, so it is asserted here too
// rather than assumed from the older test file.
func TestUnknownTypeStillFailsLoud(t *testing.T) {
	cases := []struct {
		name    string
		msg     *waE2E.Message
		rawType string
	}{
		{"encrypted reaction", &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}}, "encReactionMessage"},
		{"encrypted poll vote", &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}, "pollUpdateMessage"},
		{"request phone number", &waE2E.Message{RequestPhoneNumberMessage: &waE2E.RequestPhoneNumberMessage{}}, "requestPhoneNumberMessage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, typ := extractContent(msgEvent(tc.msg))
			if text == "" {
				t.Fatalf("SILENT DROP: %s produced an empty row; the fail-loud property "+
					"from MYC-3284 was lost", tc.rawType)
			}
			if got := unsupportedRawType(text); got != tc.rawType {
				t.Errorf("marker raw type = %q, want %q", got, tc.rawType)
			}
			if typ != "system" {
				t.Errorf("type = %q, want \"system\"", typ)
			}
		})
	}
}

// TestCarrierOnlyMessageStaysSilent: group key rotation carries no user text
// and must not gain a placeholder just because new tiers were added.
func TestCarrierOnlyMessageStaysSilent(t *testing.T) {
	text, typ := extractContent(msgEvent(&waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
	}))
	if text != "" {
		t.Errorf("text = %q, want empty: a protocol carrier must not write vault noise", text)
	}
	if typ != "system" {
		t.Errorf("type = %q, want \"system\"", typ)
	}
}

// TestRealContentBeatsCarrier: senderKeyDistributionMessage rides along with
// ordinary group messages, so it must never mask the real payload.
func TestRealContentBeatsCarrier(t *testing.T) {
	const body = "group message with a key distribution riding along"
	text, typ := extractContent(msgEvent(&waE2E.Message{
		Conversation:                 proto.String(body),
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
		MessageContextInfo:           &waE2E.MessageContextInfo{},
	}))
	if text != body || typ != "text" {
		t.Errorf("got (%q, %q), want (%q, \"text\")", text, typ, body)
	}
}

func TestUnwrapEnvelopeHandlesNil(t *testing.T) {
	if got := unwrapEnvelope(nil); got != nil {
		t.Errorf("unwrapEnvelope(nil) = %v, want nil", got)
	}
}

// TestEmptyPayloadOfDecodableTypeStillFailsLoud is the guard for the mistake
// this change made on its first attempt. Adding a decoder for a type is not the
// same as being able to READ every instance of it: a poll with no question or
// an event with no fields decodes to empty text. If the decoder claims the
// message anyway, the row is stored empty and the MYC-3284 marker is lost —
// a decode tier silently undoing the fix it was built on top of.
//
// So a decoder only claims a message when it produced text; otherwise the
// message falls through and stays loud.
func TestEmptyPayloadOfDecodableTypeStillFailsLoud(t *testing.T) {
	cases := []struct {
		name    string
		msg     *waE2E.Message
		rawType string
	}{
		{"poll with no question", &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{}}, "pollCreationMessage"},
		{"event with no fields", &waE2E.Message{EventMessage: &waE2E.EventMessage{}}, "eventMessage"},
		{"group invite with no name", &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{}}, "groupInviteMessage"},
		{"contacts array with no contacts", &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{}}, "contactsArrayMessage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, msgType := extractContent(msgEvent(tc.msg))
			if text == "" {
				t.Fatalf("SILENT DROP: %s decoded to an empty %q row — a decode tier must never "+
					"trade the fail-loud marker for a blank row", tc.rawType, msgType)
			}
			if got := unsupportedRawType(text); got != tc.rawType {
				t.Errorf("marker raw type = %q, want %q", got, tc.rawType)
			}
		})
	}
}
