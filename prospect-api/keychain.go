package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ReadDBKey returns the existing SQLCipher key.
//
// Source precedence:
//  1. PROSPECT_DB_KEY env var (escape hatch + recommended for non-darwin platforms).
//  2. Platform secret store (macOS Keychain via `security`, Linux libsecret via `secret-tool`).
//
// macOS Keychain ACLs are bound to the requesting binary's signature. The whatsapp-bridge
// binary is in the entry's trusted list; prospect-api is a different binary and triggers a
// re-authorization prompt the user may not see (e.g., when running headless or on a remote
// session). The env var path lets the user paste the key once at startup and avoid the prompt.
//
// To grab the key from Keychain manually for one-time env paste:
//
//	security find-generic-password -s whatsapp-mcp -a default -w
//
// Then export PROSPECT_DB_KEY=<that-hex>.
func ReadDBKey(service, account string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("PROSPECT_DB_KEY")); v != "" {
		if len(v) != 64 {
			return "", fmt.Errorf("PROSPECT_DB_KEY length %d (expected 64 hex chars)", len(v))
		}
		return v, nil
	}

	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("security", "find-generic-password",
			"-s", service,
			"-a", account,
			"-w",
		).Output()
		if err != nil {
			return "", fmt.Errorf("read keychain entry %s/%s: %w (set PROSPECT_DB_KEY env var to skip Keychain, or run `security find-generic-password -s %s -a %s -w` once to grant access)",
				service, account, err, service, account)
		}
		key := strings.TrimSpace(string(out))
		if len(key) != 64 {
			return "", fmt.Errorf("keychain key has unexpected length %d (expected 64 hex chars)", len(key))
		}
		return key, nil

	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return "", fmt.Errorf("secret-tool (libsecret) not found; install it or use unencrypted DB")
		}
		out, err := exec.Command("secret-tool", "lookup", "service", service, "account", account).Output()
		if err != nil {
			return "", fmt.Errorf("read secret-tool %s/%s: %w", service, account, err)
		}
		key := strings.TrimSpace(string(out))
		if len(key) != 64 {
			return "", fmt.Errorf("secret-tool key has unexpected length %d", len(key))
		}
		return key, nil

	default:
		return "", fmt.Errorf("keychain reads not implemented for %s; set PROSPECT_DB_KEY directly via env", runtime.GOOS)
	}
}
