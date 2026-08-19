//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercises the macOS key store against the REAL Keychain, mirroring
// keychain_windows_test.go.
//
// Why it asserts "returns" rather than "succeeds": the first version of this
// test asserted a clean round trip, and on the headless macos runner
// `security add-generic-password` BLOCKED — killed at 9m56s with the stack
// parked in syscall.Wait4. That is the finding, not a flake. A secret-store
// CLI that wants user authorization waits for a human forever, and this runs
// during startup, so the bridge hung before it ever listened.
//
// On a desktop Mac with an unlocked login keychain the round trip is expected
// to complete, and the extra assertions below run. On any non-interactive
// host it must instead fail FAST with a named error — which is the repo's own
// fail-loud contract, and is now enforced by keychainTimeout. What is never
// acceptable, on any host, is hanging. That is what this pins.
func TestMacOSKeychainNeverHangs(t *testing.T) {
	service := fmt.Sprintf("whatsapp-mcp-test-%d", os.Getpid())
	account := "test"
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password",
			"-s", service, "-a", account).Run()
	})

	// Short bound for the test; production default is 120s.
	restore := keychainTimeout
	keychainTimeout = 10 * time.Second
	t.Cleanup(func() { keychainTimeout = restore })

	dbPath := filepath.Join(t.TempDir(), "messages.db")

	type result struct {
		key string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		k, err := getOrCreateDBKeyMacOS(service, account, dbPath)
		done <- result{k, err}
	}()

	// Generous ceiling over the 10s internal bound: if we sit here, the
	// timeout plumbing itself is broken.
	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.err != nil {
			// Acceptable and expected on a non-interactive host — as long as
			// it is bounded and says something useful.
			t.Logf("keychain unavailable here, failed loud in %s (correct on a headless host): %v", elapsed, r.err)
			return
		}
		if len(r.key) != 64 {
			t.Fatalf("expected 64 hex chars, got %d", len(r.key))
		}
		t.Logf("real-Keychain round trip succeeded in %s", elapsed)

		// The store answered, so the interesting half is reachable: a second
		// call must READ BACK the same key, silently. If `-T ""` left the
		// item unreadable without a prompt, this is where it shows.
		key2, err := getOrCreateDBKeyMacOS(service, account, dbPath)
		if err != nil {
			t.Fatalf("write succeeded but read-back failed — the stored item is not "+
				"silently readable, which would block a real first boot: %v", err)
		}
		if key2 != r.key {
			t.Fatalf("key not stable across calls: %q != %q", r.key, key2)
		}

	case <-time.After(60 * time.Second):
		t.Fatalf("getOrCreateDBKeyMacOS did not return within 60s despite a %s internal "+
			"timeout — the bridge would hang at startup with no diagnostic", keychainTimeout)
	}
}
