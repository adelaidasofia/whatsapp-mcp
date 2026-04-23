package main

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	// SQLCipher driver. Encrypted-at-rest SQLite. See SECURITY.md for threat model.
	_ "github.com/mutecomm/go-sqlcipher/v4"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenDB opens the SQLCipher-encrypted SQLite database, creating and keying it if necessary,
// and applies any pending migrations.
func OpenDB(cfg *Config, dbKey string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}

	dsn := cfg.DBPath
	if cfg.EncryptDB {
		// SQLCipher DSN format: pass the key via PRAGMA after connect.
		// go-sqlcipher accepts the key as a URL-encoded pragma parameter.
		dsn = fmt.Sprintf("%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", cfg.DBPath, dbKey)
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlcipher db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; avoid contention.

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db (wrong key?): %w", err)
	}

	if err := applyMigrations(db); err != nil {
		return nil, fmt.Errorf("applying migrations: %w", err)
	}

	return db, nil
}

func applyMigrations(db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		// Each migration is expected to be idempotent (IF NOT EXISTS on tables, IGNORE on inserts).
		if _, err := db.Exec(string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}
