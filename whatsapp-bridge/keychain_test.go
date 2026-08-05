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

import (
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

// TestMacOSLockedKeychainReadFailsLoudAndWritesNothing is the MYC-3694
// negative control: `security find-generic-password` exiting 51
// (errSecInteractionNotAllowed — keychain locked / no UI) means the key is
// PRESENT but unreadable right now. The only correct behavior is a loud
// failure naming the remedy, with ZERO writes to the secret store.
func TestMacOSLockedKeychainReadFailsLoudAndWritesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based PATH shim; the macOS read path is exercised on the unix legs")
	}
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

	key, err := getOrCreateDBKeyMacOS("whatsapp-mcp-test-shim", "test-account")
	if err == nil {
		t.Errorf("locked-keychain read (exit 51) must fail loud, but got key %q with nil error", key)
	} else if !strings.Contains(err.Error(), "WHATSAPP_DB_KEY") {
		t.Errorf("the failure must name the WHATSAPP_DB_KEY remedy, got: %v", err)
	}
	if log := readShimLog(t, logPath); strings.Contains(log, "add-generic-password") {
		t.Errorf("secret-store WRITE after a failed read — this is the key-destroying bug:\n%s", log)
	}
}
