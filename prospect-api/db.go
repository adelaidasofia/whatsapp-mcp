package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// OpenMessageDB returns a read-mostly handle to the whatsapp-bridge's SQLCipher DB.
// We open with the same key the bridge uses (read from Keychain). The bridge keeps
// the DB connection open for writes; SQLite handles concurrent readers via WAL mode.
//
// Read paths used by prospect-api:
//   - SELECT from contacts (lookup recognition)
//   - SELECT from chats (last interaction time)
//   - SELECT from messages (recency, content scan, summary)
//
// We never write to this DB. The bridge owns writes; we treat it as a snapshot.
func OpenMessageDB(cfg *Config, dbKey string) (*sql.DB, error) {
	dsn := cfg.MessageDBPath
	if cfg.MessageDBEncrypted {
		// Same DSN format the bridge uses.
		dsn = fmt.Sprintf("%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096&mode=ro&immutable=0",
			cfg.MessageDBPath, dbKey)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open message db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping message db (key wrong, or bridge has not run yet?): %w", err)
	}
	db.SetMaxOpenConns(4) // multiple readers OK
	return db, nil
}

// OpenPresetDB returns a handle to the prospect-api's own preset DB.
// Stored unencrypted at rest because contents are short, low-sensitivity context blurbs
// + expirations. macOS FileVault still protects the file from cold-disk attackers.
// Schema is created on first run.
func OpenPresetDB(cfg *Config) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.PresetDBPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir preset db dir: %w", err)
	}
	db, err := sql.Open("sqlite3", cfg.PresetDBPath+"?_pragma_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open preset db: %w", err)
	}
	if err := initPresetSchema(db); err != nil {
		return nil, fmt.Errorf("init preset schema: %w", err)
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

func initPresetSchema(db *sql.DB) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS presets (
		preset_id TEXT PRIMARY KEY,
		guest_email TEXT,
		guest_phone TEXT,
		guest_name TEXT NOT NULL,
		context TEXT NOT NULL,
		relationship TEXT NOT NULL CHECK (relationship IN ('personal', 'professional', 'investor', 'media', 'press', 'speaking')),
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		consumed_at INTEGER,
		consumed_by TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_presets_email ON presets(guest_email) WHERE guest_email IS NOT NULL;
	CREATE INDEX IF NOT EXISTS idx_presets_phone ON presets(guest_phone) WHERE guest_phone IS NOT NULL;
	CREATE INDEX IF NOT EXISTS idx_presets_expires ON presets(expires_at);

	CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		endpoint TEXT NOT NULL,
		token_kind TEXT NOT NULL,
		client_ip TEXT,
		params_json TEXT,
		result_summary TEXT,
		duration_ms INTEGER,
		error TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_endpoint ON audit_log(endpoint, ts DESC);

	CREATE TABLE IF NOT EXISTS wa_pings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		source_endpoint TEXT NOT NULL,
		urgency TEXT NOT NULL,
		delivered INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_wa_pings_ts ON wa_pings(ts DESC);
	`
	_, err := db.Exec(schema)
	return err
}
