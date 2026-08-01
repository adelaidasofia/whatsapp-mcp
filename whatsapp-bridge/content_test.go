// content_test.go — regression fixtures for MYC-3284.
//
// The bug these tests exist to catch does not look like a bug from the inside.
// The decoder returned a valid-looking row, the insert succeeded, the counts
// went up, and the message content was gone. Nothing threw. So the tests here
// are written to fail on SILENCE, not on errors:
//
//   - TestNegativeControl_UnhandledTypeIsLoudNotSilent is the required negative
//     control. It feeds the decoder message types it does NOT handle and fails
//     if any of them come back as an empty `system` row. Re-introducing a
//     `default: return "", "system"` in decodeLeaf turns this test red.
//
//   - TestRegression_EphemeralTextWasStoredAsEmptySystem reproduces the exact
//     shape found in the live store: a plain text message inside a
//     disappearing-message envelope.

package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func textMsg(s string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(s)}
}

// --- the required negative control -----------------------------------------

// TestNegativeControl_UnhandledTypeIsLoudNotSilent is the guard the ticket
// asks for: a fixture carrying a message type the decoder does NOT handle must
// produce `unsupported` plus the raw type string, and must FAIL the test if it
// produces an empty `system` row instead.
//
// The types below are deliberately ones decodeLeaf has no case for. If a
// decoder is added for one of them later, this test will fail loudly and the
// entry should be moved into the positive-decode table — that is the intended
// workflow, not an inconvenience.
func TestNegativeControl_UnhandledTypeIsLoudNotSilent(t *testing.T) {
	cases := []struct {
		name    string
		msg     *waE2E.Message
		rawType string
	}{
		{
			name:    "request phone number",
			msg:     &waE2E.Message{RequestPhoneNumberMessage: &waE2E.RequestPhoneNumberMessage{}},
			rawType: "requestPhoneNumberMessage",
		},
		{
			// An encrypted poll vote. Genuinely undecodable without the poll
			// key — which is exactly why it must be visible rather than blank.
			name:    "poll update (encrypted vote)",
			msg:     &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}},
			rawType: "pollUpdateMessage",
		},
		{
			name:    "encrypted reaction",
			msg:     &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}},
			rawType: "encReactionMessage",
		},
		{
			name:    "interactive",
			msg:     &waE2E.Message{InteractiveMessage: &waE2E.InteractiveMessage{}},
			rawType: "interactiveMessage",
		},
		{
			name:    "template",
			msg:     &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{}},
			rawType: "templateMessage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractContentFromMessage(tc.msg)

			// The precise failure this test exists to prevent.
			if got.Type == msgTypeSystem && got.Text == "" {
				t.Fatalf("SILENT DROP: %s decoded to an empty %q row — the exact shape that "+
					"lost 46,949 messages in the live store. Undecodable messages must be "+
					"stored as %q with the raw type recorded, never as a blank system row.",
					tc.rawType, msgTypeSystem, msgTypeUnsupported)
			}
			if got.Type != msgTypeUnsupported {
				t.Errorf("type = %q, want %q", got.Type, msgTypeUnsupported)
			}
			if got.RawType != tc.rawType {
				t.Errorf("RawType = %q, want %q", got.RawType, tc.rawType)
			}
			if !got.Undecodable() {
				t.Errorf("Undecodable() = false, want true (drives /healthcheck undecoded_total)")
			}
		})
	}
}

// TestNegativeControl_UnsupportedAlwaysNamesItself guards the other half of
// "fail loud": an `unsupported` row with no raw type would be just as
// unactionable as the blank rows it replaced.
func TestNegativeControl_UnsupportedAlwaysNamesItself(t *testing.T) {
	got := ExtractContentFromMessage(&waE2E.Message{
		SecretEncryptedMessage: &waE2E.SecretEncryptedMessage{},
	})
	if got.Type != msgTypeUnsupported {
		t.Fatalf("type = %q, want %q", got.Type, msgTypeUnsupported)
	}
	if strings.TrimSpace(got.RawType) == "" {
		t.Fatal("unsupported row carries an empty RawType: nothing tells an operator what to decode next")
	}
}

// --- the original bug ------------------------------------------------------

// TestRegression_EphemeralTextWasStoredAsEmptySystem reproduces the live shape
// from MYC-3284. A text message sent in a chat with disappearing messages
// enabled arrives wrapped; the old ten-case switch saw no top-level
// conversation field and filed it as an empty system row.
func TestRegression_EphemeralTextWasStoredAsEmptySystem(t *testing.T) {
	const body = "the message that was legible in the desktop client"

	got := ExtractContentFromMessage(&waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{Message: textMsg(body)},
	})

	if got.Type == msgTypeSystem && got.Text == "" {
		t.Fatal("regression: ephemeral-wrapped text decoded to an empty system row again")
	}
	if got.Text != body {
		t.Errorf("Text = %q, want %q", got.Text, body)
	}
	if got.Type != "text" {
		t.Errorf("Type = %q, want \"text\"", got.Type)
	}
	if got.RawType != "ephemeralMessage>conversation" {
		t.Errorf("RawType = %q, want %q — the decode path is what makes the store self-describing",
			got.RawType, "ephemeralMessage>conversation")
	}
}

// --- wrapper unwrapping ----------------------------------------------------

func TestUnwrapsEveryEnvelopeType(t *testing.T) {
	const body = "wrapped payload"

	cases := []struct {
		name    string
		msg     *waE2E.Message
		rawType string
	}{
		{"ephemeral", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "ephemeralMessage>conversation"},
		{"view once", &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "viewOnceMessage>conversation"},
		{"view once v2", &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "viewOnceMessageV2>conversation"},
		{"view once v2 ext", &waE2E.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "viewOnceMessageV2Extension>conversation"},
		{"document with caption", &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "documentWithCaptionMessage>conversation"},
		{"device sent", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: textMsg(body)}}, "deviceSentMessage>conversation"},
		{"edited", &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: textMsg(body)}}, "editedMessage>conversation"},
		{"protocol edit", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{EditedMessage: textMsg(body)}}, "protocolMessage.editedMessage>conversation"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractContentFromMessage(tc.msg)
			if got.Text != body {
				t.Errorf("Text = %q, want %q", got.Text, body)
			}
			if got.Type != "text" {
				t.Errorf("Type = %q, want \"text\"", got.Type)
			}
			if got.RawType != tc.rawType {
				t.Errorf("RawType = %q, want %q", got.RawType, tc.rawType)
			}
		})
	}
}

// TestUnwrapsNestedEnvelopes covers an edit of a disappearing message — the
// realistic two-deep case.
func TestUnwrapsNestedEnvelopes(t *testing.T) {
	const body = "edited inside an ephemeral envelope"
	got := ExtractContentFromMessage(&waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				EditedMessage: &waE2E.FutureProofMessage{Message: textMsg(body)},
			},
		},
	})
	if got.Text != body {
		t.Errorf("Text = %q, want %q", got.Text, body)
	}
	if got.RawType != "ephemeralMessage>editedMessage>conversation" {
		t.Errorf("RawType = %q, want the full nested path", got.RawType)
	}
}

// TestUnwrapTerminatesOnEmptyEnvelope: a wrapper with no inner message must not
// spin, and must not be reported as text.
func TestUnwrapTerminatesOnEmptyEnvelope(t *testing.T) {
	got := ExtractContentFromMessage(&waE2E.Message{
		EphemeralMessage: &waE2E.FutureProofMessage{},
	})
	if got.Text != "" {
		t.Errorf("Text = %q, want empty", got.Text)
	}
	if got.Type != msgTypeUnsupported {
		t.Errorf("Type = %q, want %q — an envelope with nothing in it is a decode gap, not a silent skip",
			got.Type, msgTypeUnsupported)
	}
}

// --- co-occurring metadata must not be mistaken for the payload ------------

// TestSenderKeyDistributionDoesNotMaskRealContent is a high-blast-radius guard.
// senderKeyDistributionMessage rides along with ordinary group messages; if the
// classifier treated it as the message type, a large share of all group traffic
// would be misfiled.
func TestSenderKeyDistributionDoesNotMaskRealContent(t *testing.T) {
	const body = "group message with a key distribution riding along"
	got := ExtractContentFromMessage(&waE2E.Message{
		Conversation:                 proto.String(body),
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
		MessageContextInfo:           &waE2E.MessageContextInfo{},
	})
	if got.Text != body {
		t.Errorf("Text = %q, want %q", got.Text, body)
	}
	if got.Type != "text" {
		t.Errorf("Type = %q, want \"text\"", got.Type)
	}
}

// TestKeyDistributionAloneIsSilentProtocol: on its own, with no payload, it is
// a genuine protocol message and belongs in the `system` allowlist.
func TestKeyDistributionAloneIsSilentProtocol(t *testing.T) {
	got := ExtractContentFromMessage(&waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
	})
	if got.Type != msgTypeSystem {
		t.Errorf("Type = %q, want %q", got.Type, msgTypeSystem)
	}
	if got.RawType == "" || got.RawType == "empty" {
		t.Errorf("RawType = %q, want the field named even for allowlisted protocol rows", got.RawType)
	}
}

// --- newly decoded content types -------------------------------------------

func TestDecodesPollAcrossVersions(t *testing.T) {
	opts := []*waE2E.PollCreationMessage_Option{
		{OptionName: proto.String("Yes")},
		{OptionName: proto.String("No")},
	}
	poll := func() *waE2E.PollCreationMessage {
		return &waE2E.PollCreationMessage{Name: proto.String("Ship it?"), Options: opts}
	}

	cases := []struct {
		name string
		msg  *waE2E.Message
	}{
		{"v1", &waE2E.Message{PollCreationMessage: poll()}},
		{"v2", &waE2E.Message{PollCreationMessageV2: poll()}},
		{"v3", &waE2E.Message{PollCreationMessageV3: poll()}},
		{"v5", &waE2E.Message{PollCreationMessageV5: poll()}},
		{"v6", &waE2E.Message{PollCreationMessageV6: poll()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractContentFromMessage(tc.msg)
			if got.Type != "poll" {
				t.Errorf("Type = %q, want \"poll\"", got.Type)
			}
			// The question and the options are all text a human sees in the chat.
			for _, want := range []string{"Ship it?", "Yes", "No"} {
				if !strings.Contains(got.Text, want) {
					t.Errorf("Text %q missing %q", got.Text, want)
				}
			}
		})
	}
}

func TestDecodesEvent(t *testing.T) {
	got := ExtractContentFromMessage(&waE2E.Message{
		EventMessage: &waE2E.EventMessage{
			Name:        proto.String("Launch review"),
			Description: proto.String("Go/no-go"),
		},
	})
	if got.Type != "event" {
		t.Errorf("Type = %q, want \"event\"", got.Type)
	}
	if !strings.Contains(got.Text, "Launch review") || !strings.Contains(got.Text, "Go/no-go") {
		t.Errorf("Text = %q, want name and description", got.Text)
	}
}

func TestDecodesMediaCaptionsThroughWrappers(t *testing.T) {
	const caption = "caption inside a view-once envelope"
	got := ExtractContentFromMessage(&waE2E.Message{
		ViewOnceMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{Caption: proto.String(caption)},
			},
		},
	})
	if got.Type != "image" {
		t.Errorf("Type = %q, want \"image\"", got.Type)
	}
	if got.Text != caption {
		t.Errorf("Text = %q, want %q", got.Text, caption)
	}
}

// --- allowlist integrity ---------------------------------------------------

// TestSilentAllowlistEntriesAreActuallySilent asserts the `system` allowlist
// means what it claims. Every entry must classify as system — if one of these
// ever starts carrying user-visible text, it belongs in decodeLeaf instead.
//
// The `asserted` counter is not ceremony. The first version of this test used a
// magic number in place of protoreflect.MessageKind, so the kind check never
// matched, all twelve subtests called t.Skip, and the test reported PASS while
// verifying nothing — the same shape of failure as the bug under test. A
// coverage assertion is the only thing that makes a skip-based test honest.
func TestSilentAllowlistEntriesAreActuallySilent(t *testing.T) {
	// Constructed by field name via protoreflect so the test covers the
	// allowlist itself rather than a hand-copied subset that can drift.
	asserted := 0
	for field := range silentProtocolFields {
		t.Run(field, func(t *testing.T) {
			m := &waE2E.Message{}
			fd := m.ProtoReflect().Descriptor().Fields().ByJSONName(field)
			if fd == nil {
				t.Fatalf("silentProtocolFields names %q, which is not a field on waE2E.Message. "+
					"A typo here silently disables the allowlist entry.", field)
			}
			if fd.Kind() != protoreflect.MessageKind {
				t.Fatalf("%s has kind %v, expected a message field — the allowlist is keyed on "+
					"message-typed payload fields", field, fd.Kind())
			}
			m.ProtoReflect().Set(fd, m.ProtoReflect().NewField(fd))

			got := ExtractContentFromMessage(m)
			if got.Type != msgTypeSystem {
				t.Errorf("Type = %q, want %q — %s is on silentProtocolFields, which asserts it "+
					"carries no user-visible text", got.Type, msgTypeSystem, field)
			}
			asserted++
		})
	}

	if asserted != len(silentProtocolFields) {
		t.Fatalf("asserted %d of %d allowlist entries; a partially-skipped run proves nothing "+
			"about the rest", asserted, len(silentProtocolFields))
	}
}

func TestNilMessageIsHandled(t *testing.T) {
	if got := ExtractContentFromMessage(nil); got.Type != msgTypeSystem {
		t.Errorf("Type = %q, want %q", got.Type, msgTypeSystem)
	}
	if got := ExtractContent(nil); got.Type != msgTypeSystem {
		t.Errorf("Type = %q, want %q", got.Type, msgTypeSystem)
	}
}
