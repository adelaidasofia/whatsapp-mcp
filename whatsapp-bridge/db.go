package main

import (
	"database/sql"
	"embed"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	// SQLCipher driver. Encrypted-at-rest SQLite. See SECURITY.md for threat model.
	_ "github.com/mutecomm/go-sqlcipher/v4"
)

const (
	// dbMaxOpenConns bounds the pool. In WAL mode readers run concurrently, so
	// this is what lets an operator's /healthcheck probe run WHILE ingest is
	// writing instead of behind it. Small on purpose: the bridge serves one
	// local MCP server plus a health endpoint, so this is about removing
	// head-of-line blocking, not about throughput.
	dbMaxOpenConns = 8

	// dbBusyTimeoutMS is how long a statement waits for the write lock before
	// giving up with SQLITE_BUSY. WAL permits one writer at a time, and the
	// bridge has several (live receive, history sync, transcription backfill,
	// alias backfill, CRM enrich), so a pool larger than 1 makes them genuinely
	// concurrent for the first time. Every one of those writes is a single
	// short statement, so 5s is enormous headroom rather than a tuned value.
	//
	// A timeout of 0 (the previous behavior) means the FIRST contended write
	// fails instead of waiting, which is why raising the pool without this would
	// have traded a slow endpoint for intermittent write errors.
	dbBusyTimeoutMS = 5000
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenDB opens the SQLCipher-encrypted SQLite database, creating and keying it if necessary,
// and applies any pending migrations.
func OpenDB(cfg *Config, dbKey string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}

	// _busy_timeout must ride in the DSN, not an Exec: it is PER-CONNECTION, and
	// with a pool the driver has to apply it to every connection it opens. An
	// Exec would configure exactly one of them and leave the rest at 0.
	params := fmt.Sprintf("_busy_timeout=%d", dbBusyTimeoutMS)
	dsn := cfg.DBPath + "?" + params
	if cfg.EncryptDB {
		// SQLCipher DSN format: pass the key via PRAGMA after connect.
		// go-sqlcipher accepts the key as a URL-encoded pragma parameter.
		// The key is raw hex (x'...'), which puts SQLCipher in raw-key mode and
		// skips PBKDF2 entirely — that is what makes extra pooled connections
		// cheap enough to be worth having.
		dsn = fmt.Sprintf("%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096&%s", cfg.DBPath, dbKey, params)
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlcipher db: %w", err)
	}

	// The old setting here was SetMaxOpenConns(1), commented "SQLite is
	// single-writer; avoid contention." Half right, and the wrong conclusion
	// (MYC-3698). SQLite has one WRITER, but many concurrent READERS — in WAL
	// mode. Capping the pool at 1 paid the full cost of serialization and
	// collected none of the reader concurrency that would justify it: every
	// /healthcheck read queued behind whatever ingest happened to be doing.
	// Measured on the live store, ~0.5s worth of queries took 4-27s.
	//
	// Idle == open on purpose. Reopening a SQLCipher connection re-applies the
	// key pragma, so churning connections is waste when the working set is this
	// small.
	db.SetMaxOpenConns(dbMaxOpenConns)
	db.SetMaxIdleConns(dbMaxOpenConns)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db (wrong key?): %w", err)
	}

	// WAL is set HERE, by an explicit statement after Ping, rather than as a DSN
	// parameter. Ping proves the key was accepted; a DSN-ordered journal_mode
	// would be applied at a point in the driver's parameter sequence we do not
	// control, and on an encrypted database a pragma that lands before the key
	// fails. Doing it on a proven-good connection is deterministic.
	//
	// It only has to happen once ever: journal_mode is persisted in the database
	// header, so every later connection and every future run inherits it.
	// Re-running each boot is idempotent and self-healing.
	//
	// Before migrations on purpose, so a migration's own writes get WAL too.
	if mode, err := setJournalModeWAL(db); err != nil {
		// Deliberately NOT fatal. WAL is a performance property; refusing to
		// boot over it would turn a slow endpoint into an outage. It is loud in
		// the log and, more usefully, /healthcheck reports the LIVE journal_mode
		// so the state is verifiable rather than merely logged.
		log.Printf("WARN: could not enable WAL (%v) — the bridge works, but reads will serialize behind writes (MYC-3698)", err)
	} else if !strings.EqualFold(mode, "wal") {
		log.Printf("WARN: journal_mode is %q, not WAL — reads will serialize behind writes (MYC-3698). Expected on filesystems without WAL support, e.g. some network mounts", mode)
	}

	if err := applyMigrations(db); err != nil {
		return nil, fmt.Errorf("applying migrations: %w", err)
	}

	return db, nil
}

// setJournalModeWAL switches the database to write-ahead logging and returns the
// mode SQLite actually ended up in.
//
// `PRAGMA journal_mode = WAL` RETURNS a row naming the resulting mode, so this
// uses QueryRow rather than Exec. An Exec would appear to succeed while silently
// leaving the database in `delete` mode, which is exactly the silent no-op the
// caller has to be able to detect.
func setJournalModeWAL(db *sql.DB) (string, error) {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

// JournalMode reads the CURRENT journal mode from the live database. Read rather
// than remembered: the value surfaced on /healthcheck has to be the database's
// actual state, not what startup intended to set.
func JournalMode(db *sql.DB) string {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "unknown"
	}
	return mode
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
		// embed.FS keys are forward-slash only on every OS (io/fs contract).
		// filepath.Join uses "\" on Windows, which makes ReadFile fail on a
		// fresh Windows install and the bridge never migrates its DB. Use
		// path.Join here on purpose, even though the rest of this file uses
		// filepath for real on-disk paths. Regression guard: db_test.go, with
		// teeth on the windows-latest leg of tests.yml.
		sqlBytes, err := migrationsFS.ReadFile(path.Join("migrations", name))
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
