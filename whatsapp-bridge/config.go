package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all configurable runtime options, loaded from env with sensible defaults.
// Every field maps to an environment variable documented in .env.example.
type Config struct {
	BridgeHost string
	BridgePort int

	DBPath     string
	MediaPath  string
	BackupPath string
	EncryptDB  bool

	// DBKey is an explicit SQLCipher key (64 hex chars) from WHATSAPP_DB_KEY.
	// When set it overrides the platform secret store on every OS. Never logged.
	DBKey string

	KeychainService string
	KeychainAccount string

	VaultCRMPath string

	WhisperBackend   string // "local-cpp" or "openai-api"
	WhisperModel     string
	WhisperLanguage  string
	WhisperBinPath   string
	WhisperModelPath string
	WhisperAPIKey    string
	FFmpegBinPath    string
	// WhisperExcludeChats: normalized (lowercase, accent-stripped) substrings
	// matched against chat JID and chat name; matching chats are never
	// transcribed (privacy filter for personal chats).
	WhisperExcludeChats []string

	ScrubPromptInjection bool
	AuditLog             bool
	AuditLogPath         string
	AuditLogRetention    int

	WebhookURL string

	// Browser origins permitted to call the loopback API. Empty by default:
	// a non-browser client (the Python MCP server, curl, a supervisor) sends
	// no Origin header and is unaffected. Name an origin here only when a
	// browser-based UI must drive the bridge — see origin_guard.go.
	AllowedOrigins []string

	CaptureCalls      bool
	CaptureReactions  bool
	AutoDownloadMedia bool

	// Voice-note transcript backfill sweeper. Periodically re-enqueues any
	// voice/audio rows with media keys but NULL transcripts, so transient
	// failures self-heal instead of leaving permanent NULLs.
	BackfillIntervalSeconds int
	BackfillWindowDays      int
}

// LoadConfig reads every env var, applies defaults, and returns a populated Config.
// Blank env values are treated as unset (fall back to default).
// Any bind address other than a loopback is rejected at startup.
func LoadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	defaultRoot := filepath.Join(home, ".claude", "whatsapp-mcp")

	c := &Config{
		BridgeHost:      getenv("WHATSAPP_BRIDGE_HOST", "127.0.0.1"),
		BridgePort:      getenvInt("WHATSAPP_BRIDGE_PORT", 8080),
		DBPath:          expandPath(getenv("WHATSAPP_DB_PATH", filepath.Join(defaultRoot, "store", "messages.db")), home),
		MediaPath:       expandPath(getenv("WHATSAPP_MEDIA_PATH", filepath.Join(defaultRoot, "media")), home),
		BackupPath:      expandPath(getenv("WHATSAPP_BACKUP_PATH", filepath.Join(defaultRoot, "store", "backups")), home),
		EncryptDB:       getenvBool("WHATSAPP_ENCRYPT_DB", true),
		DBKey:           getenv("WHATSAPP_DB_KEY", ""),
		KeychainService: getenv("WHATSAPP_KEYCHAIN_SERVICE", "whatsapp-mcp"),
		KeychainAccount: getenv("WHATSAPP_KEYCHAIN_ACCOUNT", "default"),
		VaultCRMPath:    expandPath(getenv("WHATSAPP_VAULT_CRM_PATH", ""), home),
		// "off" by default so a fresh install boots with zero config. The
		// old default (local-cpp) demanded a ~3 GB whisper model at startup
		// — Config.Validate refused to boot without it, stranding every new
		// install. Transcription stays fail-loud once explicitly enabled.
		WhisperBackend:          getenv("WHATSAPP_WHISPER_BACKEND", "off"),
		WhisperModel:            getenv("WHATSAPP_WHISPER_MODEL", "large-v3"),
		WhisperLanguage:         getenv("WHATSAPP_WHISPER_LANGUAGE", "es"),
		WhisperBinPath:          getenv("WHATSAPP_WHISPER_BIN_PATH", ""),
		WhisperModelPath:        expandPath(getenv("WHATSAPP_WHISPER_MODEL_PATH", ""), home),
		WhisperAPIKey:           getenv("WHATSAPP_WHISPER_API_KEY", ""),
		WhisperExcludeChats:     splitNormalizedCSV(getenv("WHATSAPP_WHISPER_EXCLUDE_CHATS", "")),
		FFmpegBinPath:           getenv("WHATSAPP_FFMPEG_BIN_PATH", ""),
		ScrubPromptInjection:    getenvBool("WHATSAPP_SCRUB_PROMPT_INJECTION", true),
		AuditLog:                getenvBool("WHATSAPP_AUDIT_LOG", true),
		AuditLogPath:            expandPath(getenv("WHATSAPP_AUDIT_LOG_PATH", filepath.Join(defaultRoot, "audit.log")), home),
		AuditLogRetention:       getenvInt("WHATSAPP_AUDIT_LOG_RETENTION_DAYS", 30),
		WebhookURL:              getenv("WHATSAPP_WEBHOOK_URL", ""),
		AllowedOrigins:          splitCSV(getenv("WHATSAPP_ALLOWED_ORIGINS", "")),
		CaptureCalls:            getenvBool("WHATSAPP_CAPTURE_CALLS", true),
		CaptureReactions:        getenvBool("WHATSAPP_CAPTURE_REACTIONS", true),
		AutoDownloadMedia:       getenvBool("WHATSAPP_AUTO_DOWNLOAD_MEDIA", false),
		BackfillIntervalSeconds: getenvInt("WHATSAPP_BACKFILL_INTERVAL_SECONDS", 900),
		BackfillWindowDays:      getenvInt("WHATSAPP_BACKFILL_WINDOW_DAYS", 14),
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate enforces non-negotiable security constraints at startup.
func (c *Config) Validate() error {
	// SECURITY: bridge must bind to a loopback address only.
	if !isLoopback(c.BridgeHost) {
		return fmt.Errorf("WHATSAPP_BRIDGE_HOST=%q is not a loopback address; only 127.0.0.1 or ::1 allowed", c.BridgeHost)
	}
	// Port 0 = OS-assigned; the bound port is announced via a
	// "BRIDGE_LISTENING port=N" stdout line + a bridge.port sidecar file so
	// a supervisor running several isolated instances can discover it.
	if c.BridgePort < 0 || c.BridgePort > 65535 {
		return fmt.Errorf("WHATSAPP_BRIDGE_PORT=%d out of range", c.BridgePort)
	}
	if c.WhisperBackend != "off" && c.WhisperBackend != "local-cpp" && c.WhisperBackend != "openai-api" {
		return fmt.Errorf("WHATSAPP_WHISPER_BACKEND=%q must be 'off', 'local-cpp' or 'openai-api'", c.WhisperBackend)
	}
	if c.WhisperBackend == "openai-api" && c.WhisperAPIKey == "" {
		return fmt.Errorf("WHATSAPP_WHISPER_BACKEND=openai-api requires WHATSAPP_WHISPER_API_KEY to be set")
	}
	// Fail loud at startup rather than silently per-job in the transcriber.
	// Earlier behavior: missing model path → every voice note dropped with
	// only a debug-level log line, no signal at boot.
	if c.WhisperBackend == "local-cpp" {
		if c.WhisperModelPath == "" {
			return fmt.Errorf("WHATSAPP_WHISPER_MODEL_PATH is required for local-cpp backend (set to absolute path of ggml-*.bin)")
		}
		if _, err := os.Stat(c.WhisperModelPath); err != nil {
			return fmt.Errorf("WHATSAPP_WHISPER_MODEL_PATH=%s not readable: %w", c.WhisperModelPath, err)
		}
	}
	if c.AuditLogRetention < 1 {
		return fmt.Errorf("WHATSAPP_AUDIT_LOG_RETENTION_DAYS must be >= 1")
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}

// splitNormalizedCSV parses a comma-separated env value into normalized
// (lowercase, accent-stripped via Normalize) non-empty patterns.
// splitCSV splits a comma-separated list WITHOUT running Normalize over it.
// splitNormalizedCSV exists for chat names, where accent folding is the point;
// applying it to a URL is semantically wrong and would corrupt an IDN origin.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitNormalizedCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(Normalize(strings.TrimSpace(p))); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// expandPath expands a leading ~ or $HOME into the actual home directory,
// and resolves relative paths against the current working directory.
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
