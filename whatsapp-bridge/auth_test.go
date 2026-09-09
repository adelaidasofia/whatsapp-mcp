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

// TestFatalIfDeviceDeletedWiring covers the half TestIsDeviceDeleted cannot
// reach: that the predicate is actually wired to the shutdown request. An
// adversarial review pointed out that asserting errors.Is on a sentinel is close
// to tautological — the standard library guarantees it — while the thing that
// had genuinely been wrong in this change was the wiring around it.
func TestFatalIfDeviceDeletedWiring(t *testing.T) {
	b := &Bridge{fatal: make(chan struct{})}

	closed := func() bool {
		select {
		case <-b.Fatal():
			return true
		default:
			return false
		}
	}

	// Must NOT request shutdown: nil, and transients that would otherwise
	// restart the bridge on every flaky network blip or stale-socket round.
	b.fatalIfDeviceDeleted(nil)
	b.fatalIfDeviceDeleted(context.DeadlineExceeded)
	b.fatalIfDeviceDeleted(errors.New("qr channel: already connected"))
	if closed() {
		t.Fatal("requested shutdown on a non-sentinel error")
	}

	// Must request shutdown, wrapped exactly the way loginLoop wraps it.
	b.fatalIfDeviceDeleted(fmt.Errorf("connect for pairing: %w", store.ErrDeviceDeleted))
	if !closed() {
		t.Fatal("the sentinel did not reach the shutdown request")
	}

	// Idempotent: several handlers can reach the same conclusion concurrently,
	// and a second close of the channel would panic.
	b.fatalIfDeviceDeleted(fmt.Errorf("again: %w", store.ErrDeviceDeleted))
	b.requestFatalShutdown()
}
