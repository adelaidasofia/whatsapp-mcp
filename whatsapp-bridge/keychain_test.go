package main

// MYC-3694: a transient secret-store READ failure must never be treated as
// "no key yet". The old flow minted a fresh key on ANY read error and wrote
// it with overwrite semantics (`security add-generic-password -U`), silently
// replacing the real SQLCipher key and permanently orphaning the encrypted
// message store.
//
// These tests never touch the real keychain: PATH is pinned to a temp dir
// containing ONLY a fake `security` / `secret-tool`, so the real binaries are
// unreachable, and all service names are test-only. The fake logs every
// invocation so "zero writes" is asserted, not assumed.
//
// The shared-flow tests (Test_KeyStore*) run on every OS leg, including
// Windows; the PATH-shim subprocess tests run on the unix legs. Windows-only
// tests against the real Credential Manager live in keychain_windows_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installShim installs an executable fake <name> in a fresh temp dir and pins
// PATH to ONLY that dir for the duration of the test, so exec.Command(name,
// ...) cannot fall through to the real binary. Every invocation appends its
// argv to the returned log file before the scripted behavior runs.
func installShim(t *testing.T, name, body string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "invocations.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return logPath
}

func readShimLog(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "" // shim never invoked
		}
		t.Fatal(err)
	}
	return string(b)
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sh-based PATH shim; this exec path is exercised on the unix legs")
	}
}

// absentStorePath returns a message-store path that does not exist, so mint
// decisions in these tests are driven purely by the read classification.
func absentStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "messages.db")
}

// populatedStorePath returns a message-store path holding a non-empty file,
// standing in for a real encrypted store.
func populatedStorePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "messages.db")
	if err := os.WriteFile(p, []byte("SQLCipher-encrypted bytes stand-in"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// recordingStore builds a keyStore whose read yields a fixed outcome and
// whose write records every key it is asked to persist.
func recordingStore(key string, res keyReadResult, detail error, writes *[]string) keyStore {
	return keyStore{
		name:   "test secret store",
		remedy: "set WHATSAPP_DB_KEY to the original key",
		read:   func() (string, keyReadResult, error) { return key, res, detail },
		write:  func(k string) error { *writes = append(*writes, k); return nil },
	}
}

// --- Shared decision flow (runs on all three OS legs) ---

func Test_KeyStore_ReadErrorFailsLoudAndNeverWrites(t *testing.T) {
	var writes []string
	s := recordingStore("", keyReadError, errors.New("exit 51: User interaction is not allowed."), &writes)

	key, err := s.getOrCreate(absentStorePath(t))
	if err == nil {
		t.Fatalf("a failed read must fail loud, got key %q", key)
	}
	if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the failure must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if len(writes) != 0 {
		t.Errorf("a failed read must write NOTHING, wrote %v", writes)
	}
}

func Test_KeyStore_ProvenNotFoundMints(t *testing.T) {
	var writes []string
	s := recordingStore("", keyReadNotFound, nil, &writes)

	key, err := s.getOrCreate(absentStorePath(t))
	if err != nil {
		t.Fatalf("a proven not-found on a fresh install must mint: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key))
	}
	if len(writes) != 1 || writes[0] != key {
		t.Fatalf("expected exactly one write of the minted key, got %v", writes)
	}
}

func Test_KeyStore_MintRefusedOverPopulatedStore(t *testing.T) {
	var writes []string
	s := recordingStore("", keyReadNotFound, nil, &writes)

	key, err := s.getOrCreate(populatedStorePath(t))
	if err == nil {
		t.Fatalf("minting over an existing non-empty store must be refused, got key %q", key)
	}
	if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the refusal must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if len(writes) != 0 {
		t.Errorf("the refusal path must write NOTHING, wrote %v", writes)
	}
}

func Test_KeyStore_ExistingKeyReturnedWithoutWrites(t *testing.T) {
	var writes []string
	existing := strings.Repeat("ab", 32)
	s := recordingStore(existing, keyReadOK, nil, &writes)

	key, err := s.getOrCreate(populatedStorePath(t))
	if err != nil {
		t.Fatalf("a readable existing key must be used as-is: %v", err)
	}
	if key != existing {
		t.Fatalf("expected the stored key back, got %q", key)
	}
	if len(writes) != 0 {
		t.Errorf("reading an existing key must write NOTHING, wrote %v", writes)
	}
}

func Test_KeyStore_MalformedKeyIsNeverOverwritten(t *testing.T) {
	var writes []string
	s := recordingStore("tooshort", keyReadOK, nil, &writes)

	key, err := s.getOrCreate(absentStorePath(t))
	if err == nil {
		t.Fatalf("a present-but-malformed key must fail loud (old code overwrote it), got key %q", key)
	}
	if len(writes) != 0 {
		t.Errorf("a malformed key must never be overwritten, wrote %v", writes)
	}
}

func Test_StoreGuardBeforeMint(t *testing.T) {
	if err := storeGuardBeforeMint("test store", ""); err != nil {
		t.Errorf("empty dbPath (no store configured) must allow minting: %v", err)
	}
	if err := storeGuardBeforeMint("test store", absentStorePath(t)); err != nil {
		t.Errorf("absent store file must allow minting: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "messages.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storeGuardBeforeMint("test store", empty); err != nil {
		t.Errorf("zero-byte placeholder store must allow minting: %v", err)
	}
	if err := storeGuardBeforeMint("test store", populatedStorePath(t)); err == nil {
		t.Errorf("non-empty store must refuse minting")
	}
}

// --- macOS `security` classification via PATH shim (unix legs) ---

// TestMacOSLockedKeychainReadFailsLoudAndWritesNothing is the MYC-3694
// negative control: `security find-generic-password` exiting 51
// (errSecInteractionNotAllowed — keychain locked / no UI) means the key is
// PRESENT but unreadable right now. The only correct behavior is a loud
// failure naming the remedy, with ZERO writes to the secret store.
//
// Captured against the unfixed code (commit 150a4fb): the bridge minted a
// fresh key and invoked `add-generic-password ... -U` — the overwrite that
// permanently orphans the encrypted store.
func TestMacOSLockedKeychainReadFailsLoudAndWritesNothing(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "security", `
case "$1" in
  find-generic-password)
    echo "security: SecKeychainSearchCopyNext: User interaction is not allowed." >&2
    exit 51
    ;;
  add-generic-password)
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyMacOS("whatsapp-mcp-test-shim", "test-account", absentStorePath(t))
	if err == nil {
		t.Errorf("locked-keychain read (exit 51) must fail loud, but got key %q with nil error", key)
	} else if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the failure must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if log := readShimLog(t, logPath); strings.Contains(log, "add-generic-password") {
		t.Errorf("secret-store WRITE after a failed read — this is the key-destroying bug:\n%s", log)
	}
}

// A proven not-found (exit 44, errSecItemNotFound) must still mint on a
// genuine first run — fresh-install pairing depends on it — and the write
// must carry NO overwrite flag (-U): create-only, so a concurrent item fails
// the write instead of being silently replaced.
func TestMacOSProvenNotFoundMintsCreateOnly(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "security", `
case "$1" in
  find-generic-password)
    echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2
    exit 44
    ;;
  add-generic-password)
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyMacOS("whatsapp-mcp-test-shim", "test-account", absentStorePath(t))
	if err != nil {
		t.Fatalf("proven not-found on a fresh install must mint: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key))
	}
	log := readShimLog(t, logPath)
	if n := strings.Count(log, "add-generic-password"); n != 1 {
		t.Fatalf("expected exactly one keychain write, got %d:\n%s", n, log)
	}
	if !strings.Contains(log, key) {
		t.Errorf("the write must store the returned key\n%s", log)
	}
	if strings.Contains(log, "-U") {
		t.Errorf("the write must be create-only (no -U overwrite flag):\n%s", log)
	}
}

// Even a proven not-found must not mint while a populated store exists: the
// key went missing, and a fresh key could never decrypt the existing rows.
func TestMacOSMintRefusedWhenStoreFileExists(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "security", `
case "$1" in
  find-generic-password)
    exit 44
    ;;
  add-generic-password)
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyMacOS("whatsapp-mcp-test-shim", "test-account", populatedStorePath(t))
	if err == nil {
		t.Errorf("minting over an existing store must be refused, got key %q", key)
	} else if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the refusal must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if log := readShimLog(t, logPath); strings.Contains(log, "add-generic-password") {
		t.Errorf("the refusal path must write NOTHING:\n%s", log)
	}
}

// --- Linux `secret-tool` classification via PATH shim (unix legs) ---

// A lookup that fails WITH stderr output (no D-Bus, locked collection) is a
// read error, not a miss: fail loud, zero writes.
func TestLinuxLookupErrorFailsLoudAndWritesNothing(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "secret-tool", `
case "$1" in
  lookup)
    echo "secret-tool: Cannot autolaunch D-Bus without X11 \$DISPLAY" >&2
    exit 1
    ;;
  store)
    cat > /dev/null
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyLinux("whatsapp-mcp-test-shim", "test-account", absentStorePath(t))
	if err == nil {
		t.Errorf("a failed secret-tool lookup must fail loud, but got key %q with nil error", key)
	} else if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the failure must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if log := readShimLog(t, logPath); strings.Contains(log, "store") {
		t.Errorf("secret-store WRITE after a failed read:\n%s", log)
	}
}

// secret-tool's only proven-absent signal is exit 1 with a SILENT stderr;
// that (and only that) still mints on a genuine first run.
func TestLinuxSilentMissMints(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "secret-tool", `
case "$1" in
  lookup)
    exit 1
    ;;
  store)
    cat > /dev/null
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyLinux("whatsapp-mcp-test-shim", "test-account", absentStorePath(t))
	if err != nil {
		t.Fatalf("silent miss (proven not-found) on a fresh install must mint: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(key))
	}
	if n := strings.Count(readShimLog(t, logPath), "store"); n != 1 {
		t.Fatalf("expected exactly one secret-tool store call, got %d", n)
	}
}

// Same store-file guard on the Linux path: proven not-found + populated store
// → refuse, zero writes.
func TestLinuxMintRefusedWhenStoreFileExists(t *testing.T) {
	skipOnWindows(t)
	logPath := installShim(t, "secret-tool", `
case "$1" in
  lookup)
    exit 1
    ;;
  store)
    cat > /dev/null
    exit 0
    ;;
esac
exit 0`)

	key, err := getOrCreateDBKeyLinux("whatsapp-mcp-test-shim", "test-account", populatedStorePath(t))
	if err == nil {
		t.Errorf("minting over an existing store must be refused, got key %q", key)
	}
	if log := readShimLog(t, logPath); strings.Contains(log, "store") {
		t.Errorf("the refusal path must write NOTHING:\n%s", log)
	}
}
