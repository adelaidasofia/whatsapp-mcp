//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"
)

// Round-trip against the real Windows Credential Manager. Runs on the
// windows-latest CI runner — this is the proof the Windows key store works,
// not just compiles.
func TestWindowsCredentialRoundTrip(t *testing.T) {
	service := fmt.Sprintf("whatsapp-mcp-test-%d", os.Getpid())
	account := "test"
	target := credTarget(service, account)
	t.Cleanup(func() { _ = winCredDelete(target) })

	// First call generates + stores a fresh 64-hex-char key.
	key1, err := getOrCreateDBKeyWindows(service, account)
	if err != nil {
		t.Fatalf("first getOrCreateDBKeyWindows: %v", err)
	}
	if len(key1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key1))
	}

	// Second call must return the SAME stored key, not a new one.
	key2, err := getOrCreateDBKeyWindows(service, account)
	if err != nil {
		t.Fatalf("second getOrCreateDBKeyWindows: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("key not stable across calls: %q != %q", key1, key2)
	}

	// Raw read agrees.
	raw, err := winCredRead(target)
	if err != nil {
		t.Fatalf("winCredRead: %v", err)
	}
	if raw != key1 {
		t.Fatalf("stored blob mismatch")
	}

	// Delete works and a subsequent read fails.
	if err := winCredDelete(target); err != nil {
		t.Fatalf("winCredDelete: %v", err)
	}
	if _, err := winCredRead(target); err == nil {
		t.Fatalf("expected read-after-delete to fail")
	}
}
