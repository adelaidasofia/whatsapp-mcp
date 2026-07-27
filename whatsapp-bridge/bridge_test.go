package main

import "testing"

// TestChatNameFromMessage pins the rule that a sender's name is not a chat's
// name.
//
// The outgoing case is the one that produced the reported bug: `list_chats`
// returned the account owner's own name for ten distinct @lid chats, because
// evt.Info.PushName on an outgoing message describes the sender — who, for an
// outgoing message, is the user.
func TestChatNameFromMessage(t *testing.T) {
	tests := []struct {
		name     string
		isGroup  bool
		isFromMe bool
		pushName string
		want     string
	}{
		// The single case where the sender and the chat are the same human.
		{"incoming dm names the chat", false, false, "Diego Lancon", "Diego Lancon"},

		{"outgoing dm contributes nothing", false, true, "Jonathan", ""},
		{"incoming group message contributes nothing", true, false, "Diego Lancon", ""},
		{"outgoing group message contributes nothing", true, true, "Jonathan", ""},

		// Better no label than an opaque one: sender_display falls back to the
		// JID's user part, which for an @lid is a meaningless number, and the
		// JID is already on the row for a caller that wants to render it.
		{"empty push name yields no name", false, false, "", ""},
		{"whitespace-only push name yields no name", false, false, "   ", ""},
		{"push name is trimmed", false, false, "  Diego Lancon  ", "Diego Lancon"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chatNameFromMessage(tc.isGroup, tc.isFromMe, tc.pushName)
			if got != tc.want {
				t.Fatalf("chatNameFromMessage(isGroup=%v, isFromMe=%v, %q) = %q, want %q",
					tc.isGroup, tc.isFromMe, tc.pushName, got, tc.want)
			}
		})
	}
}
