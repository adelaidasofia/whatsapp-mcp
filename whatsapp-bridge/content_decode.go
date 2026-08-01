// content_decode.go — envelope unwrapping and the second tier of content
// decoders, layered on top of extractContent (MYC-3284 follow-up).
//
// MYC-3284 made an undecodable message LOUD: instead of collapsing to an empty
// `system` row, it keeps a queryable "[unsupported: <rawType>]" marker naming
// the proto field. That fixed the visibility half of the bug — the loss is no
// longer silent.
//
// This file addresses the other half: several of the types now showing up as
// markers are not actually undecodable, they were simply never decoded. Read
// off the live bridge after MYC-3284 shipped:
//
//	[unsupported: albumMessage]           12
//	[unsupported: pollCreationMessageV3]   5
//	[unsupported: liveLocationMessage]     2
//	[unsupported: contactsArrayMessage]    1
//
// Each of those carries text a human reads in the chat. A marker is the right
// answer for a type we genuinely cannot read; it is the wrong answer for one we
// simply have not written a decoder for yet.
//
// The larger correctness gap is ENVELOPES. whatsmeow nests the real message
// inside a wrapper for disappearing messages, view-once, device-sent,
// document-with-caption and edits. extractContent tests only top-level fields,
// so a plain text message sent in a chat with disappearing messages enabled has
// no top-level `conversation` and falls through to the default branch. Today
// that stores "[unsupported: ephemeralMessage]" — visible, but the text itself
// is still not captured. Unwrapping first means such a message decodes as what
// it actually is.
//
// Nothing here weakens the fail-loud property. Unwrapping runs BEFORE the
// existing switch, the new decoders run AFTER it, and anything still unmatched
// falls through to the same default branch and the same marker as before.
//
// Types map onto the existing messages.type CHECK values (a poll is stored as
// `text`, a live location as `location`), so this needs no migration and no
// change to the CHECK constraint — deliberately, per the reasoning recorded in
// extractContent's default branch.

package main

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// maxUnwrapDepth bounds envelope descent. Real nesting is one or two deep (an
// edit of a disappearing message); the bound is here so a malformed or hostile
// payload cannot spin the decoder.
const maxUnwrapDepth = 8

// unwrapEnvelope descends wrapper types that nest the real message and returns
// the innermost payload. Returns the original message unchanged when it is not
// an envelope, so callers can apply it unconditionally.
func unwrapEnvelope(m *waE2E.Message) *waE2E.Message {
	for depth := 0; m != nil && depth < maxUnwrapDepth; depth++ {
		var inner *waE2E.Message
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			inner = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			inner = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			inner = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			inner = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			inner = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetLottieStickerMessage().GetMessage() != nil:
			inner = m.GetLottieStickerMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			inner = m.GetDeviceSentMessage().GetMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			inner = m.GetEditedMessage().GetMessage()
		// An edit delivered as a protocol message carries the replacement body,
		// so the store ends up holding the edited text rather than a bare
		// protocol row.
		case m.GetProtocolMessage().GetEditedMessage() != nil:
			inner = m.GetProtocolMessage().GetEditedMessage()
		}
		if inner == nil {
			return m
		}
		m = inner
	}
	return m
}

// decodeExtraContent handles content types the main switch does not, returning
// ok=false when it has nothing to offer so the caller falls through to the
// unchanged fail-loud default.
//
// msgType values are restricted to those already permitted by the
// messages.type CHECK constraint.
func decodeExtraContent(m *waE2E.Message) (text, msgType string, ok bool) {
	if m == nil {
		return "", "", false
	}

	switch {
	// Polls. Six versions ship concurrently and all but V4 share this shape.
	// The question and its options are text a human sees in the chat.
	case m.GetPollCreationMessage() != nil:
		return pollText(m.GetPollCreationMessage()), "text", true
	case m.GetPollCreationMessageV2() != nil:
		return pollText(m.GetPollCreationMessageV2()), "text", true
	case m.GetPollCreationMessageV3() != nil:
		return pollText(m.GetPollCreationMessageV3()), "text", true
	case m.GetPollCreationMessageV5() != nil:
		return pollText(m.GetPollCreationMessageV5()), "text", true
	case m.GetPollCreationMessageV6() != nil:
		return pollText(m.GetPollCreationMessageV6()), "text", true

	case m.GetEventMessage() != nil:
		return eventText(m.GetEventMessage()), "text", true

	case m.GetLiveLocationMessage() != nil:
		return m.GetLiveLocationMessage().GetCaption(), "location", true

	case m.GetContactsArrayMessage() != nil:
		return contactsArrayText(m.GetContactsArrayMessage()), "contact", true

	case m.GetGroupInviteMessage() != nil:
		return joinNonEmpty("\n",
			m.GetGroupInviteMessage().GetGroupName(),
			m.GetGroupInviteMessage().GetCaption()), "text", true

	// Video note (the round "view once"-style video). Same payload as a video.
	case m.GetPtvMessage() != nil:
		return m.GetPtvMessage().GetCaption(), "video", true

	// Commerce and interactive. These render as text in the client, so the
	// text is what we store.
	case m.GetProductMessage() != nil:
		return productText(m.GetProductMessage()), "text", true
	case m.GetOrderMessage() != nil:
		return firstNonEmpty(m.GetOrderMessage().GetMessage(), m.GetOrderMessage().GetOrderTitle()), "text", true
	case m.GetListMessage() != nil:
		return firstNonEmpty(m.GetListMessage().GetDescription(), m.GetListMessage().GetTitle()), "text", true
	case m.GetListResponseMessage() != nil:
		return firstNonEmpty(m.GetListResponseMessage().GetTitle(), m.GetListResponseMessage().GetDescription()), "text", true
	case m.GetButtonsMessage() != nil:
		return firstNonEmpty(m.GetButtonsMessage().GetContentText(), m.GetButtonsMessage().GetText()), "text", true
	case m.GetButtonsResponseMessage() != nil:
		return m.GetButtonsResponseMessage().GetSelectedDisplayText(), "text", true
	case m.GetTemplateButtonReplyMessage() != nil:
		return m.GetTemplateButtonReplyMessage().GetSelectedDisplayText(), "text", true
	case m.GetInteractiveResponseMessage() != nil:
		return m.GetInteractiveResponseMessage().GetBody().GetText(), "text", true
	}

	return "", "", false
}

// --- per-type text builders ------------------------------------------------

func pollText(p *waE2E.PollCreationMessage) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.GetName())
	for _, opt := range p.GetOptions() {
		if name := opt.GetOptionName(); name != "" {
			b.WriteString("\n- ")
			b.WriteString(name)
		}
	}
	return b.String()
}

func eventText(e *waE2E.EventMessage) string {
	if e == nil {
		return ""
	}
	parts := []string{e.GetName(), e.GetDescription(), e.GetLocation().GetName()}
	if e.GetIsCanceled() {
		parts = append(parts, "(canceled)")
	}
	return joinNonEmpty("\n", parts...)
}

func productText(p *waE2E.ProductMessage) string {
	if p == nil {
		return ""
	}
	snap := p.GetProduct()
	return joinNonEmpty("\n", snap.GetTitle(), snap.GetDescription(), p.GetBody())
}

func contactsArrayText(c *waE2E.ContactsArrayMessage) string {
	if c == nil {
		return ""
	}
	names := []string{c.GetDisplayName()}
	for _, ct := range c.GetContacts() {
		names = append(names, ct.GetDisplayName())
	}
	return joinNonEmpty("\n", names...)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinNonEmpty(sep string, vals ...string) string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}
