package main

import "testing"

// TestSeedChatName covers the direct-chat "IsFromMe poisons the name
// forever" bug: onMessage's INSERT seeds chats.name once and the ON
// CONFLICT clause never updates it again, so the very first write decides
// the chat's identity for its whole lifetime.
func TestSeedChatName(t *testing.T) {
	cases := []struct {
		name          string
		senderDisplay string
		chatType      string
		isFromMe      bool
		want          string
	}{
		{
			name:          "direct chat, first message from the other party",
			senderDisplay: "Jane Doe",
			chatType:      "direct",
			isFromMe:      false,
			want:          "Jane Doe",
		},
		{
			name:          "direct chat, first message is our own -> blank, not our own name",
			senderDisplay: "Device Owner",
			chatType:      "direct",
			isFromMe:      true,
			want:          "",
		},
		{
			name:          "group chat, first message is our own -> left alone",
			senderDisplay: "Device Owner",
			chatType:      "group",
			isFromMe:      true,
			want:          "Device Owner",
		},
		{
			name:          "group chat, first message from someone else -> left alone",
			senderDisplay: "Jane Doe",
			chatType:      "group",
			isFromMe:      false,
			want:          "Jane Doe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := seedChatName(tc.senderDisplay, tc.chatType, tc.isFromMe)
			if got != tc.want {
				t.Errorf("seedChatName(%q, %q, %v) = %q, want %q",
					tc.senderDisplay, tc.chatType, tc.isFromMe, got, tc.want)
			}
		})
	}
}
