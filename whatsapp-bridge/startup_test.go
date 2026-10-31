package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// holdAPIPort plays the bridge that is already running: it owns a loopback
// port, and the bridge under test is pointed at it. It also points the bridge
// at a throwaway home so these tests can never reach the real store or
// Keychain, and returns the directory OpenDB would create first.
func holdAPIPort(t *testing.T) (storeDir string) {
	t.Helper()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	t.Cleanup(func() { held.Close() })

	home := t.TempDir()
	storeDir = filepath.Join(home, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("WHATSAPP_BRIDGE_HOST", "127.0.0.1")
	t.Setenv("WHATSAPP_BRIDGE_PORT", strconv.Itoa(held.Addr().(*net.TCPAddr).Port))
	t.Setenv("WHATSAPP_DB_PATH", filepath.Join(storeDir, "messages.db"))
	t.Setenv("WHATSAPP_MEDIA_PATH", filepath.Join(home, "media"))
	t.Setenv("WHATSAPP_AUDIT_LOG_PATH", filepath.Join(home, "audit.log"))
	t.Setenv("WHATSAPP_ENCRYPT_DB", "false") // no Keychain, even if one of these tests starts failing
	t.Setenv("WHATSAPP_DB_KEY", "")
	t.Setenv("WHATSAPP_VAULT_CRM_PATH", "")
	t.Setenv("WHATSAPP_WHISPER_BACKEND", "off")
	return storeDir
}

// captureLog keeps run's log lines out of the test output unless a test fails.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// A second copy of the bridge, started while one already serves the API port,
// must give up at the port: before it opens the store (ReconcileInFlightSends
// marks the live copy's in-flight sends failed) and before it connects to
// WhatsApp (which kicks the live copy's session off). A launchd service started
// next to a hand-started copy used to connect and only then die on "address
// already in use", 154 times in a row.
//
// run is called exactly as main calls it. The store directory is the witness:
// OpenDB creates it first thing, and NewBridge (so RunAuth) cannot be reached
// without the database it opens.
//
// The exit code must be non-zero: the launchd and systemd units restart only
// an unsuccessful exit, which is how the service takes over once the other copy
// stops.
func TestSecondBridgeCopyExitsBeforeTouchingWhatsAppOrStore(t *testing.T) {
	storeDir := holdAPIPort(t)
	logs := captureLog(t)

	if code := run(nil); code == 0 {
		t.Fatalf("exit code 0 although the API port was taken; the service manager would not retry\n%s", logs)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the store was opened (stat err = %v) before the bridge knew it could serve the API; "+
			"a second copy gets that far, then connects to WhatsApp and kicks the first copy off\n%s", err, logs)
	}
}

// The offline modes never serve and never connect, and they are meant to run
// next to the live bridge: the vault export runs this binary every few hours
// while the service holds the port. Claiming the port for them would turn
// every one of those runs into a failure.
func TestOfflineExportRunsWhileAnotherBridgeServes(t *testing.T) {
	holdAPIPort(t)
	logs := captureLog(t)

	out := filepath.Join(t.TempDir(), "vault")
	if code := run([]string{"--export-to-vault", out}); code != 0 {
		t.Fatalf("export exited %d while another bridge held the API port\n%s", code, logs)
	}
}
