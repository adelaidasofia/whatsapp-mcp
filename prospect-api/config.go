package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the prospect-api runtime configuration.
//
// All values are loaded from environment variables with sensible defaults.
// Bind address is loopback-only enforced at startup; the public surface is
// the Cloudflare Tunnel sitting in front of this loopback bind.
//
// Token semantics:
//   - BridgeAuthToken: bearer token for the concierge service (server-to-server).
//   - AdminToken: separate token for /api/preset only. Strictly higher trust.
type Config struct {
	BindHost string
	BindPort int

	// Path to the whatsapp-bridge's encrypted SQLite DB (read-only access for messages/contacts).
	MessageDBPath string

	// Path to the prospect-api's own preset DB (separate, simpler, opaque to message data).
	PresetDBPath string

	// Whether the message DB is SQLCipher-encrypted (it is; bridge owns it).
	MessageDBEncrypted bool

	// Keychain coordinates (shared with the whatsapp-bridge).
	KeychainService string
	KeychainAccount string

	// Vault CRM folder. Required for lookup/pull-context/update-crm.
	VaultCRMPath string

	// Inbox output folder. Required for relay-note (Sprint 7+).
	VaultInboxPath string

	// Negotiator playbook path. Required for get-negotiator-terms (Sprint 10).
	NegotiatorPlaybookPath string

	// whatsapp-bridge HTTP base URL (used by notify-owner and relay-note for sends).
	BridgeBaseURL string

	// Auth tokens.
	BridgeAuthToken string
	AdminToken      string

	// Rate limits.
	GlobalRateLimitPerMin int // 60 default per spec
	WhatsAppPingsPerHour  int // 10 default; shared budget across notify-owner + relay-note

	// Audit log.
	AuditLogPath string

	// Quiet hours for WhatsApp pings (high-urgency arriving in window are queued to the configured wake hour).
	QuietHoursStart    string // "22:00"
	QuietHoursEnd      string // "07:00"
	QuietHoursOverride bool   // env var to suppress quiet-hours behavior

	// Cloudflare Access trust: when behind Cloudflare Access, we can additionally
	// require the cf-access-jwt header to be present (validated by Cloudflare upstream).
	RequireCloudflareAccess bool

	// Time zone for cron + quiet hours. Default is the maintainer's TZ
	// (overridable via PROSPECT_TIMEZONE; consumers in other regions should set this).
	TimeZone string
}

func LoadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	defaultRoot := filepath.Join(home, ".claude", "whatsapp-mcp")

	c := &Config{
		BindHost:                getenv("PROSPECT_BIND_HOST", "127.0.0.1"),
		BindPort:                getenvInt("PROSPECT_BIND_PORT", 8081),
		MessageDBPath:           expandPath(getenv("WHATSAPP_DB_PATH", filepath.Join(defaultRoot, "store", "messages.db")), home),
		PresetDBPath:            expandPath(getenv("PROSPECT_PRESET_DB_PATH", filepath.Join(defaultRoot, "prospect-api", "store", "presets.db")), home),
		MessageDBEncrypted:      getenvBool("WHATSAPP_ENCRYPT_DB", true),
		KeychainService:         getenv("WHATSAPP_KEYCHAIN_SERVICE", "whatsapp-mcp"),
		KeychainAccount:         getenv("WHATSAPP_KEYCHAIN_ACCOUNT", "default"),
		VaultCRMPath:            expandPath(getenv("WHATSAPP_VAULT_CRM_PATH", ""), home),
		VaultInboxPath:          expandPath(getenv("PROSPECT_VAULT_INBOX_PATH", ""), home),
		NegotiatorPlaybookPath:  expandPath(getenv("PROSPECT_NEGOTIATOR_PLAYBOOK_PATH", ""), home),
		BridgeBaseURL:           getenv("PROSPECT_BRIDGE_BASE_URL", "http://127.0.0.1:8080"),
		BridgeAuthToken:         os.Getenv("PROSPECT_BRIDGE_AUTH_TOKEN"),
		AdminToken:              os.Getenv("PROSPECT_ADMIN_TOKEN"),
		GlobalRateLimitPerMin:   getenvInt("PROSPECT_RATE_LIMIT_PER_MIN", 60),
		WhatsAppPingsPerHour:    getenvInt("PROSPECT_WA_PINGS_PER_HOUR", 10),
		AuditLogPath:            expandPath(getenv("PROSPECT_AUDIT_LOG_PATH", filepath.Join(defaultRoot, "prospect-api", "audit.log")), home),
		QuietHoursStart:         getenv("PROSPECT_QUIET_HOURS_START", "22:00"),
		QuietHoursEnd:           getenv("PROSPECT_QUIET_HOURS_END", "07:00"),
		QuietHoursOverride:      getenvBool("PROSPECT_QUIET_HOURS_OVERRIDE", false),
		RequireCloudflareAccess: getenvBool("PROSPECT_REQUIRE_CF_ACCESS", false),
		TimeZone:                getenv("PROSPECT_TIMEZONE", "UTC"),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) Validate() error {
	if !isLoopback(c.BindHost) {
		return fmt.Errorf("PROSPECT_BIND_HOST=%q must be a loopback address (127.0.0.1 or ::1); public exposure is via Cloudflare Tunnel only", c.BindHost)
	}
	if c.BindPort < 1 || c.BindPort > 65535 {
		return fmt.Errorf("PROSPECT_BIND_PORT=%d out of range", c.BindPort)
	}
	if c.BridgeAuthToken == "" {
		return errors.New("PROSPECT_BRIDGE_AUTH_TOKEN is required (the concierge service uses this for bearer auth)")
	}
	if c.AdminToken == "" {
		return errors.New("PROSPECT_ADMIN_TOKEN is required (separate token for /api/preset)")
	}
	if c.AdminToken == c.BridgeAuthToken {
		return errors.New("PROSPECT_ADMIN_TOKEN must differ from PROSPECT_BRIDGE_AUTH_TOKEN; admin endpoints have higher trust")
	}
	if c.VaultCRMPath == "" {
		return errors.New("WHATSAPP_VAULT_CRM_PATH is required for prospect lookup/CRM endpoints")
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}

func getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func expandPath(p, home string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	if strings.HasPrefix(p, "${HOME}/") {
		return filepath.Join(home, p[len("${HOME}/"):])
	}
	if strings.HasPrefix(p, "$HOME/") {
		return filepath.Join(home, p[len("$HOME/"):])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
