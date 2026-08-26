package main

import (
	"testing"
	"time"
)

// TestNextNudgeDelayEscalates pins the retry schedule. The risk being bounded
// here is not the outage — it is the fix: an earlier review of the supervisor
// calculated that ~109k authentication attempts per week from a single device
// risks an account-level block, which would be far worse than the disconnection
// this recovers from.
func TestNextNudgeDelayEscalates(t *testing.T) {
	// The first half hour is where a transient cause (DNS blip, TLS timeout, a
	// laptop resuming from sleep) actually clears, so attempts are close
	// together.
	for n := 0; n < 6; n++ {
		if got := nextNudgeDelay(n); got != 5*time.Minute {
			t.Fatalf("nudge %d: delay = %s, want 5m", n, got)
		}
	}
	// After that the cause is probably structural and no retry rate will fix
	// it, so back off hard and stay there.
	for _, n := range []int{6, 7, 20, 500} {
		if got := nextNudgeDelay(n); got != 30*time.Minute {
			t.Fatalf("nudge %d: delay = %s, want 30m", n, got)
		}
	}

	// The budget that keeps this safe to leave running for months.
	var firstHour time.Duration
	attempts := 0
	for firstHour < time.Hour {
		firstHour += nextNudgeDelay(attempts)
		attempts++
	}
	if attempts > 12 {
		t.Fatalf("%d attempts in the first hour is too many; the schedule must stay conservative", attempts)
	}
	// A day of a permanently-rejected client must stay in the tens, not thousands.
	perDayAfterBackoff := int(24 * time.Hour / nextNudgeDelay(99))
	if perDayAfterBackoff > 60 {
		t.Fatalf("%d attempts/day once backed off is too many", perDayAfterBackoff)
	}
}

// TestDisconnectedForReportsDuration covers the state the watchdog reads and
// /api/status reports. `connected: false` alone cannot distinguish a two-second
// blip from the 24-hour outage that motivated this, which is exactly why the
// outage went unnoticed.
func TestDisconnectedForReportsDuration(t *testing.T) {
	b := &Bridge{}

	// Never connected: not an outage to report. Startup and pairing have their
	// own states, and reporting "down forever" before the first connection
	// would fire on every cold start.
	if got := b.DisconnectedFor(); got != 0 {
		t.Fatalf("fresh bridge: DisconnectedFor = %s, want 0", got)
	}

	// Connected: nothing to report even if a stale timestamp lingers.
	b.connected = true
	b.disconnectedSince = time.Now().Add(-time.Hour)
	if got := b.DisconnectedFor(); got != 0 {
		t.Fatalf("connected bridge: DisconnectedFor = %s, want 0", got)
	}

	// Down: the duration has to be real, not a boolean in disguise.
	b.connected = false
	b.disconnectedSince = time.Now().Add(-90 * time.Minute)
	got := b.DisconnectedFor()
	if got < 89*time.Minute || got > 91*time.Minute {
		t.Fatalf("DisconnectedFor = %s, want ~90m", got)
	}
	if got < watchdogGrace {
		t.Fatal("a 90-minute outage must be past the grace period")
	}
}

// TestDisconnectClockStartsOncePerOutage is the subtle one. whatsmeow emits
// events.Disconnected repeatedly while it retries; if each one reset the clock,
// the measured duration would never exceed a single retry interval and a
// day-long outage would look permanently fresh — reproducing the exact blindness
// this change exists to remove.
func TestDisconnectClockStartsOncePerOutage(t *testing.T) {
	b := &Bridge{}

	// The handler's logic: set only when unset.
	markDown := func() {
		b.connected = false
		if b.disconnectedSince.IsZero() {
			b.disconnectedSince = time.Now()
		}
	}
	markUp := func() {
		b.connected = true
		b.disconnectedSince = time.Time{}
	}

	markDown()
	first := b.disconnectedSince
	b.disconnectedSince = first.Add(-30 * time.Minute) // simulate 30m elapsed
	pinned := b.disconnectedSince

	// Several more Disconnected events, as whatsmeow retries.
	for i := 0; i < 5; i++ {
		markDown()
	}
	if !b.disconnectedSince.Equal(pinned) {
		t.Fatal("repeat disconnects reset the clock; a long outage would look fresh forever")
	}
	if got := b.DisconnectedFor(); got < 29*time.Minute {
		t.Fatalf("DisconnectedFor = %s, want ~30m after repeat disconnects", got)
	}

	// Reconnecting clears it, so the next outage is measured from its own start
	// and the watchdog's backoff restarts at the fast end.
	markUp()
	if got := b.DisconnectedFor(); got != 0 {
		t.Fatalf("after reconnect: DisconnectedFor = %s, want 0", got)
	}
	markDown()
	if got := b.DisconnectedFor(); got > time.Minute {
		t.Fatalf("a new outage should start near zero, got %s", got)
	}
}
