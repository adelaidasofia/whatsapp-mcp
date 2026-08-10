package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow/store"
)

// TestIsDeviceDeleted pins the real incident: a real two-day outage where the
// bridge silently sat logged-out because the code assumed a re-login on the
// same *whatsmeow.Client would work after WhatsApp force-logged the device out
// from another device. It never can — whatsmeow deletes the device row for any
// such logout (connectionevents.go), and every subsequent call on that client
// fails identically. The check has to match whatsmeow's own sentinel exactly,
// not a copy of its message: matching the message and the library changing its
// wording would silently stop detecting the one case this exists for.
func TestIsDeviceDeleted(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"the actual sentinel", store.ErrDeviceDeleted, true},
		{
			"wrapped the way loginLoop wraps it",
			fmt.Errorf("connect for pairing: %w", store.ErrDeviceDeleted),
			true,
		},
		{
			"wrapped twice, the way Reconnect's caller sees it",
			fmt.Errorf("login loop ended: %w", fmt.Errorf("connect for pairing: %w", store.ErrDeviceDeleted)),
			true,
		},
		// Must stay narrow. A transient failure mistakenly matching this would
		// restart the bridge for no reason on every flaky network blip.
		{"context deadline (a slow network, not a deleted device)", context.DeadlineExceeded, false},
		{"an unrelated whatsmeow error", errors.New("qr channel: some other failure"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeviceDeleted(tc.err); got != tc.want {
				t.Fatalf("isDeviceDeleted(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
