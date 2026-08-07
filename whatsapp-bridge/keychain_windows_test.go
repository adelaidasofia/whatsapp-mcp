//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// No message store yet, so minting on a proven not-found is allowed.
	dbPath := filepath.Join(t.TempDir(), "messages.db")

	// First call generates + stores a fresh 64-hex-char key.
	key1, err := getOrCreateDBKeyWindows(service, account, dbPath)
	if err != nil {
		t.Fatalf("first getOrCreateDBKeyWindows: %v", err)
	}
	if len(key1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key1))
	}

	// Second call must return the SAME stored key, not a new one.
	key2, err := getOrCreateDBKeyWindows(service, account, dbPath)
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

// MYC-3694: a missing credential must classify as PROVEN not-found — the only
// state that may mint — via ERROR_NOT_FOUND from the real CredReadW, not be
// lumped in with read errors.
func TestWindowsMissingCredentialClassifiesNotFound(t *testing.T) {
	target := credTarget(fmt.Sprintf("whatsapp-mcp-test-missing-%d", os.Getpid()), "test")
	_, res, detail := winClassifiedRead(target)
	if res != keyReadNotFound {
		t.Fatalf("expected keyReadNotFound for a missing credential, got res=%d detail=%v", res, detail)
	}
}

// MYC-3694: even a proven not-found must not mint while a populated message
// store exists — and the refusal must leave the real Credential Manager
// untouched (the target still reads as not-found afterwards).
func TestWindowsMintRefusedWhenStoreFileExists(t *testing.T) {
	service := fmt.Sprintf("whatsapp-mcp-test-storeguard-%d", os.Getpid())
	account := "test"
	target := credTarget(service, account)
	t.Cleanup(func() { _ = winCredDelete(target) })

	dbPath := filepath.Join(t.TempDir(), "messages.db")
	if err := os.WriteFile(dbPath, []byte("SQLCipher-encrypted bytes stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := getOrCreateDBKeyWindows(service, account, dbPath)
	if err == nil {
		t.Fatalf("expected refusal to mint over an existing store, got key %q", key)
	}
	if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Fatalf("the refusal must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}

	if _, res, detail := winClassifiedRead(target); res != keyReadNotFound {
		t.Fatalf("the refusal path must write NOTHING to the credential store; target now reads res=%d detail=%v", res, detail)
	}
}
