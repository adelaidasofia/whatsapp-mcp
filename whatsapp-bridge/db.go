package main

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// Read which versions have already been applied. Errors here just mean
	// the schema_version table doesn't exist yet (very first boot); 001
	// creates it idempotently and we run 001 unconditionally.
	applied := map[int]bool{}
	if rows, err := db.Query(`SELECT version FROM schema_version`); err == nil {
		for rows.Next() {
			var v int
			if scanErr := rows.Scan(&v); scanErr == nil {
				applied[v] = true
			}
		}
		rows.Close()
	}

	for _, name := range names {
		v := parseMigrationVersion(name)
		// 001 is fully idempotent (IF NOT EXISTS / OR IGNORE). 002+ contain
		// non-idempotent DDL like ALTER TABLE ADD COLUMN, so skip when the
		// version is already recorded.
		if v >= 2 && applied[v] {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.Exec(string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// parseMigrationVersion extracts the leading integer from a filename like
// "002_voice_backfill.sql" → 2. Returns 0 for unparseable names so the
// loader treats them as "always run" (matches 001 historic behavior).
func parseMigrationVersion(name string) int {
	end := 0
	for end < len(name) && name[end] >= '0' && name[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.Atoi(name[:end])
	if err != nil {
		return 0
	}
	return v
}
