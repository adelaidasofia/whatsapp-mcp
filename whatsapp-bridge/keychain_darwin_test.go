//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Round-trip against the REAL macOS Keychain, mirroring
// keychain_windows_test.go. Runs on the macos CI runner.
//
// Why this exists: keychain_test.go covers the macOS branch with a PATH shim,
// which proves the exit-code CLASSIFICATION (the MYC-3694 three-state
// contract) but never touches a real keychain. Windows had a real-store test
// and macOS did not, so the one key store proven end-to-end was the one most
// users do not run.
//
// The specific thing under test is the `-T ""` argument on the write. Its
// documented effect is to remove the default trust the creating application
// would get, which — if it applies to /usr/bin/security here — would make
// every subsequent read require user authorization. CI runners are
// non-interactive with no UI available, so a read that needs a prompt CANNOT
// silently pass: it fails, and this test is where that surfaces. A green run
// is the evidence that first boot on a Mac does not hang on a keychain
// dialog.
func TestMacOSKeychainRoundTripAgainstRealStore(t *testing.T) {
	service := fmt.Sprintf("whatsapp-mcp-test-%d", os.Getpid())
	account := "test"
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password",
			"-s", service, "-a", account).Run()
	})

	// No message store yet, so minting on a proven not-found is allowed.
	dbPath := filepath.Join(t.TempDir(), "messages.db")

	// First call must mint + store a fresh 64-hex-char key.
	key1, err := getOrCreateDBKeyMacOS(service, account, dbPath)
	if err != nil {
		t.Fatalf("first getOrCreateDBKeyMacOS against the real keychain: %v", err)
	}
	if len(key1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key1))
	}

	// Second call must READ BACK the same key with no prompt. This is the
	// assertion that matters: if the item's ACL denies /usr/bin/security a
	// silent read, this call fails here rather than in a user's terminal.
	key2, err := getOrCreateDBKeyMacOS(service, account, dbPath)
	if err != nil {
		t.Fatalf("second getOrCreateDBKeyMacOS (read-back) failed — the stored "+
			"item is not silently readable, which is exactly what would block "+
			"a first boot: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("key not stable across calls: %q != %q", key1, key2)
	}

	// A direct read through the same CLI the bridge uses must agree.
	out, err := exec.Command("security", "find-generic-password",
		"-s", service, "-a", account, "-w").Output()
	if err != nil {
		t.Fatalf("direct find-generic-password: %v", execErrDetail(err))
	}
	if got := string(trimTrailingNewline(out)); got != key1 {
		t.Fatalf("stored secret mismatch: %q != %q", got, key1)
	}
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
