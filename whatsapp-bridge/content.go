// content.go — turning a waE2E.Message into the (text, type) pair we store.
//
// The bug this file exists to prevent (MYC-3284): extractContent used to be a
// ten-case switch ending in `default: return "", "system"`. Every message shape
// WhatsApp introduced after that switch was written fell into the default
// branch and was persisted as a type:"system" row with empty content_text — no
// error, no log line, and nothing at all distinguishing "this message had no
// text" from "I could not decode this message". When it was finally caught,
// 46,949 of 113,449 rows in the live store were empty `system` (41.4%, across
// 399 chats, six months), and 45,564 of those had no same-second sibling of any
// other type — whole messages, not protocol echoes of real ones.
//
// Two structural changes, not just more cases:
//
//  1. Wrappers are unwrapped, not defaulted. Disappearing messages, view-once,
//     device-sent, document-with-caption and edits all carry the real message
//     nested inside another Message. Reaching the payload is a loop, not a case
//     — so a plain text message inside a disappearing-message envelope decodes
//     as text, which is what it is.
//
//  2. `system` is a positive allowlist, not a fallback. A row is only stored as
//     `system` when its protobuf field is on silentProtocolFields: types that
//     carry no user-visible text by design. Anything the decoder does not
//     understand becomes `unsupported`, keeps the raw protobuf field name, and
//     logs a WARN. Unknown is loud. Silence has to be earned.
//
// The unknown branch is deliberately driven by protoreflect rather than a
// hand-maintained list of all 103 Message fields. A hand-maintained list is the
// same failure mode one level up: it goes stale silently the next time
// WhatsApp ships a message type, and nothing tells us. Asking the payload which
// field it actually populated cannot go stale, and it yields the raw type
// string for free.

package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Content is the decoded form of one WhatsApp message.
type Content struct {
	// Text is the user-visible text, or "" for types that legitimately carry
	// none (a sticker, a voice note). Empty Text is only ever meaningful in
	// combination with Type.
	Text string

	// Type is the value stored in messages.type. Constrained by the CHECK in
	// migration 004.
	Type string

	// RawType is the decode path: the wrapper chain plus the terminal protobuf
	// field, e.g. "ephemeralMessage>conversation", or "pollCreationMessageV3".
	// Stored in messages.raw_type on every row so that "what was this really?"
	// is answerable from the store instead of by re-deriving from a live client.
	RawType string
}

// Undecodable reports whether this message's payload was not understood.
// These are the rows /healthcheck counts and the vault export renders as an
// explicit placeholder.
func (c Content) Undecodable() bool { return c.Type == msgTypeUnsupported }

const (
	msgTypeUnsupported = "unsupported"
	msgTypeSystem      = "system"

	// maxUnwrapDepth bounds wrapper descent. Real nesting is 1-2 deep
	// (an edit of a disappearing message); the bound exists so a malformed or
	// hostile payload cannot spin the decoder.
	maxUnwrapDepth = 8
)

// coOccurringFields ride along with real content and must never be mistaken
// for the payload. senderKeyDistributionMessage in particular is attached to
// ordinary group messages, so treating it as the message type would misfile
// a large share of group traffic.
var coOccurringFields = map[string]bool{
	"messageContextInfo":                         true,
	"senderKeyDistributionMessage":               true,
	"fastRatchetKeySenderKeyDistributionMessage": true,
}

// silentProtocolFields carry no user-visible text by design. This is the ONLY
// path to a `system` row. Adding an entry here is a claim that a human reading
// the chat in WhatsApp would see no text for this message — if that is wrong,
// the message belongs in `unsupported` where it is visible and countable.
var silentProtocolFields = map[string]bool{
	"senderKeyDistributionMessage":               true,
	"fastRatchetKeySenderKeyDistributionMessage": true,
	"messageContextInfo":                         true,
	"protocolMessage":                            true, // revoke / ephemeral-setting / app-state
	"pinInChatMessage":                           true,
	"keepInChatMessage":                          true,
	"placeholderMessage":                         true,
	"stickerSyncRmrMessage":                      true,
	"stickerPackMessage":                         true,
	"albumMessage":                               true, // container; the children arrive as their own messages
	"eventCoverImage":                            true,
	"statusNotificationMessage":                  true,
}

// ExtractContent decodes a live-receive event.
func ExtractContent(evt *events.Message) Content {
	if evt == nil {
		return Content{Type: msgTypeSystem, RawType: "empty"}
	}
	return ExtractContentFromMessage(evt.Message)
}

// ExtractContentFromMessage decodes a bare waE2E.Message. Live receive and
// history-sync backfill both reach this, so a message re-delivered by history
// sync decodes byte-identically to how it would have decoded live — which is
// what makes the backfill trustworthy rather than a second, drifting decoder.
func ExtractContentFromMessage(m *waE2E.Message) Content {
	if m == nil {
		return Content{Type: msgTypeSystem, RawType: "empty"}
	}

	inner, path := unwrap(m)
	text, typ, leaf := decodeLeaf(inner)

	raw := leaf
	if len(path) > 0 {
		raw = strings.Join(path, ">") + ">" + leaf
	}

	if typ == msgTypeUnsupported {
		warnUndecodable(raw)
	}
	return Content{Text: text, Type: typ, RawType: raw}
}

// unwrap descends through envelope types that nest the real message, returning
// the innermost payload plus the wrapper chain that got us there.
//
// Disappearing messages are the volume case: an ordinary text message sent in a
// chat with disappearing messages enabled arrives as
// ephemeralMessage{message:{conversation:"hi"}}. The old switch saw no
// conversation at the top level and filed it as empty `system`.
func unwrap(m *waE2E.Message) (*waE2E.Message, []string) {
	var path []string
	for depth := 0; m != nil && depth < maxUnwrapDepth; depth++ {
		var (
			inner *waE2E.Message
			label string
		)
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			inner, label = m.GetEphemeralMessage().GetMessage(), "ephemeralMessage"
		case m.GetViewOnceMessage().GetMessage() != nil:
			inner, label = m.GetViewOnceMessage().GetMessage(), "viewOnceMessage"
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			inner, label = m.GetViewOnceMessageV2().GetMessage(), "viewOnceMessageV2"
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			inner, label = m.GetViewOnceMessageV2Extension().GetMessage(), "viewOnceMessageV2Extension"
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			inner, label = m.GetDocumentWithCaptionMessage().GetMessage(), "documentWithCaptionMessage"
		case m.GetLottieStickerMessage().GetMessage() != nil:
			inner, label = m.GetLottieStickerMessage().GetMessage(), "lottieStickerMessage"
		case m.GetDeviceSentMessage().GetMessage() != nil:
			inner, label = m.GetDeviceSentMessage().GetMessage(), "deviceSentMessage"
		case m.GetEditedMessage().GetMessage() != nil:
			inner, label = m.GetEditedMessage().GetMessage(), "editedMessage"
		// An edit delivered as a protocol message carries the replacement body.
		// Unwrapping it means the store holds the edited text rather than an
		// empty protocol row.
		case m.GetProtocolMessage().GetEditedMessage() != nil:
			inner, label = m.GetProtocolMessage().GetEditedMessage(), "protocolMessage.editedMessage"
		}
		if inner == nil {
			return m, path
		}
		path = append(path, label)
		m = inner
	}
	return m, path
}

// decodeLeaf reads the innermost payload. Returns the text, the storage type,
// and the protobuf field name it decoded (or the populated-field summary when
// it could not).
func decodeLeaf(m *waE2E.Message) (text, typ, leaf string) {
	if m == nil {
		return "", msgTypeSystem, "empty"
	}

	switch {
	// --- text ---
	case m.GetConversation() != "":
		return m.GetConversation(), "text", "conversation"
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), "text", "extendedTextMessage"

	// --- media (text is the caption, where one exists) ---
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), "image", "imageMessage"
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption(), "video", "videoMessage"
	case m.GetPtvMessage() != nil:
		// Video note ("view once"-style round video). Same payload as a video.
		return m.GetPtvMessage().GetCaption(), "video", "ptvMessage"
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "", "voice", "audioMessage"
		}
		return "", "audio", "audioMessage"
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption(), "document", "documentMessage"
	case m.GetStickerMessage() != nil:
		return "", "sticker", "stickerMessage"

	// --- location ---
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetComment(), "location", "locationMessage"
	case m.GetLiveLocationMessage() != nil:
		return m.GetLiveLocationMessage().GetCaption(), "location", "liveLocationMessage"

	// --- contacts ---
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetDisplayName(), "contact", "contactMessage"
	case m.GetContactsArrayMessage() != nil:
		return contactsArrayText(m.GetContactsArrayMessage()), "contact", "contactsArrayMessage"

	case m.GetReactionMessage() != nil:
		return m.GetReactionMessage().GetText(), "reaction", "reactionMessage"

	// --- polls. Six versions ship concurrently; all but V4 share the same
	// payload shape, and V4 wraps one. The question and the options are text a
	// human sees in the chat, so they are text we store. ---
	case m.GetPollCreationMessage() != nil:
		return pollText(m.GetPollCreationMessage()), "poll", "pollCreationMessage"
	case m.GetPollCreationMessageV2() != nil:
		return pollText(m.GetPollCreationMessageV2()), "poll", "pollCreationMessageV2"
	case m.GetPollCreationMessageV3() != nil:
		return pollText(m.GetPollCreationMessageV3()), "poll", "pollCreationMessageV3"
	case m.GetPollCreationMessageV5() != nil:
		return pollText(m.GetPollCreationMessageV5()), "poll", "pollCreationMessageV5"
	case m.GetPollCreationMessageV6() != nil:
		return pollText(m.GetPollCreationMessageV6()), "poll", "pollCreationMessageV6"

	// --- events ---
	case m.GetEventMessage() != nil:
		return eventText(m.GetEventMessage()), "event", "eventMessage"

	// --- group invites ---
	case m.GetGroupInviteMessage() != nil:
		return groupInviteText(m.GetGroupInviteMessage()), "text", "groupInviteMessage"

	// --- commerce + interactive. These carry text a human reads in the chat,
	// so they are stored as text rather than thrown away. ---
	case m.GetProductMessage() != nil:
		return productText(m.GetProductMessage()), "text", "productMessage"
	case m.GetOrderMessage() != nil:
		return firstNonEmpty(m.GetOrderMessage().GetMessage(), m.GetOrderMessage().GetOrderTitle()), "text", "orderMessage"
	case m.GetListMessage() != nil:
		return firstNonEmpty(m.GetListMessage().GetDescription(), m.GetListMessage().GetTitle()), "text", "listMessage"
	case m.GetListResponseMessage() != nil:
		return firstNonEmpty(m.GetListResponseMessage().GetTitle(), m.GetListResponseMessage().GetDescription()), "text", "listResponseMessage"
	case m.GetButtonsMessage() != nil:
		return firstNonEmpty(m.GetButtonsMessage().GetContentText(), m.GetButtonsMessage().GetText()), "text", "buttonsMessage"
	case m.GetButtonsResponseMessage() != nil:
		return m.GetButtonsResponseMessage().GetSelectedDisplayText(), "text", "buttonsResponseMessage"
	case m.GetTemplateButtonReplyMessage() != nil:
		return m.GetTemplateButtonReplyMessage().GetSelectedDisplayText(), "text", "templateButtonReplyMessage"
	case m.GetInteractiveResponseMessage() != nil:
		return m.GetInteractiveResponseMessage().GetBody().GetText(), "text", "interactiveResponseMessage"
	case m.GetCommentMessage() != nil:
		return ExtractContentFromMessage(m.GetCommentMessage().GetMessage()).Text, "text", "commentMessage"
	}

	// Unknown. Ask the payload what it actually is rather than guessing.
	typ, leaf = classify(m)
	return "", typ, leaf
}

// classify names the populated fields and decides between the `system`
// allowlist and a loud `unsupported`.
func classify(m *waE2E.Message) (typ, leaf string) {
	fields := populatedFields(m, true)
	if len(fields) == 0 {
		// Nothing but envelope metadata. Re-read including the co-occurring
		// fields so the row still records what was seen instead of "empty".
		fields = populatedFields(m, false)
	}
	if len(fields) == 0 {
		return msgTypeSystem, "empty"
	}

	leaf = strings.Join(fields, "+")
	for _, f := range fields {
		if !silentProtocolFields[f] {
			return msgTypeUnsupported, leaf
		}
	}
	return msgTypeSystem, leaf
}

// populatedFields returns the JSON names of the fields actually set on the
// message, sorted for stability. When excludeCoOccurring is true, the metadata
// fields that ride along with real content are omitted.
func populatedFields(m *waE2E.Message, excludeCoOccurring bool) []string {
	if m == nil {
		return nil
	}
	var out []string
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		name := fd.JSONName()
		if excludeCoOccurring && coOccurringFields[name] {
			return true
		}
		out = append(out, name)
		return true
	})
	sort.Strings(out)
	return out
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
	parts := []string{e.GetName()}
	if d := e.GetDescription(); d != "" {
		parts = append(parts, d)
	}
	if l := e.GetLocation().GetName(); l != "" {
		parts = append(parts, l)
	}
	if e.GetIsCanceled() {
		parts = append(parts, "(canceled)")
	}
	return strings.Join(nonEmpty(parts), "\n")
}

func groupInviteText(g *waE2E.GroupInviteMessage) string {
	if g == nil {
		return ""
	}
	return strings.Join(nonEmpty([]string{g.GetGroupName(), g.GetCaption()}), "\n")
}

func productText(p *waE2E.ProductMessage) string {
	if p == nil {
		return ""
	}
	snap := p.GetProduct()
	return strings.Join(nonEmpty([]string{
		snap.GetTitle(),
		snap.GetDescription(),
		p.GetBody(),
	}), "\n")
}

func contactsArrayText(c *waE2E.ContactsArrayMessage) string {
	if c == nil {
		return ""
	}
	names := []string{c.GetDisplayName()}
	for _, ct := range c.GetContacts() {
		names = append(names, ct.GetDisplayName())
	}
	return strings.Join(nonEmpty(names), "\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(vals []string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// --- fail-loud logging -----------------------------------------------------

var undecodedSeen sync.Map // rawType -> *undecodedCounter

type undecodedCounter struct {
	mu sync.Mutex
	n  int64
}

// warnUndecodable logs the first sighting of every raw type immediately, then
// every 100th, so a newly-shipped WhatsApp message type is named the moment it
// appears without a high-volume type flooding the log. Exact counts live in
// /healthcheck (undecoded_by_type); this is the "tell me now" channel, not the
// accounting one.
func warnUndecodable(rawType string) {
	v, _ := undecodedSeen.LoadOrStore(rawType, &undecodedCounter{})
	c := v.(*undecodedCounter)
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()

	if n == 1 || n%100 == 0 {
		log.Printf("WARN: undecodable message type %q stored as type=%q (seen %d time(s) this process). "+
			"Content was NOT captured. Add a decoder in content.go decodeLeaf, or add it to "+
			"silentProtocolFields if it genuinely carries no user-visible text.",
			rawType, msgTypeUnsupported, n)
	}
}

// undecodableSummary renders the per-type counts this process has seen.
// Used by tests and by operators reading logs.
func undecodableSummary() string {
	var parts []string
	undecodedSeen.Range(func(k, v any) bool {
		c := v.(*undecodedCounter)
		c.mu.Lock()
		n := c.n
		c.mu.Unlock()
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		return true
	})
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
