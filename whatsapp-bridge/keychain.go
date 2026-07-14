package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// GetOrCreateDBKey returns a hex-encoded 256-bit key for SQLCipher.
// On first run, a random key is generated and stored in the platform secret
// store. On subsequent runs, the existing key is returned. The user is
// prompted by the OS when the store is first accessed (macOS).
//
// macOS: Keychain via `security`. Linux: libsecret via `secret-tool`.
// Windows: Credential Manager via advapi32 (keychain_windows.go).
//
// An explicit WHATSAPP_DB_KEY env var overrides all of these — main.go checks
// it BEFORE calling here (the escape hatch for headless/CI/custom setups).
//
// The key is never logged, never written to disk outside the platform secret store,
// never committed to the repo.
func GetOrCreateDBKey(service, account string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return getOrCreateDBKeyMacOS(service, account)
	case "linux":
		return getOrCreateDBKeyLinux(service, account)
	case "windows":
		return getOrCreateDBKeyWindows(service, account)
	default:
		return "", fmt.Errorf("no secret store for %s; set WHATSAPP_DB_KEY (64 hex chars) directly", runtime.GOOS)
	}
}

func getOrCreateDBKeyMacOS(service, account string) (string, error) {
	// Try to read existing key.
	out, err := exec.Command("security", "find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	).Output()
	if err == nil {
		key := strings.TrimSpace(string(out))
		if len(key) == 64 { // 32 bytes hex-encoded
			return key, nil
		}
		// Key exists but wrong length; fall through to replace.
	}

	// Generate fresh key.
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random key: %w", err)
	}
	key := hex.EncodeToString(b)

	// Store in Keychain. -U flag updates if an entry exists.
	// The user is prompted to authorize Keychain access the first time.
	cmd := exec.Command("security", "add-generic-password",
		"-s", service,
		"-a", account,
		"-w", key,
		"-T", "", // restrict access to this binary path; empty = default ACL
		"-U",
	)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("write key to keychain: %w", err)
	}
	return key, nil
}

func getOrCreateDBKeyLinux(service, account string) (string, error) {
	// Prefer libsecret (GNOME Keyring / KDE KWallet).
	if _, err := exec.LookPath("secret-tool"); err == nil {
		out, err := exec.Command("secret-tool", "lookup", "service", service, "account", account).Output()
		if err == nil {
			key := strings.TrimSpace(string(out))
			if len(key) == 64 {
				return key, nil
			}
		}

		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generate random key: %w", err)
		}
		key := hex.EncodeToString(b)

		// secret-tool store needs stdin.
		cmd := exec.Command("secret-tool", "store",
			"--label", fmt.Sprintf("%s db key", service),
			"service", service, "account", account)
		cmd.Stdin = strings.NewReader(key)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("write key via secret-tool: %w", err)
		}
		return key, nil
	}

	return "", fmt.Errorf("secret-tool (libsecret) not found; install it or set WHATSAPP_DB_KEY directly")
}
