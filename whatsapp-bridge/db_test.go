package main

import (
	"database/sql"
	"path"
	"path/filepath"
	"testing"
)

// Regression coverage for the migration loader.
//
// The bug (v0.2.0, fresh Windows install): applyMigrations read each embedded
// migration with filepath.Join("migrations", name). embed.FS keys are
// forward-slash only on every OS, but filepath.Join uses "\" on Windows, so
// ReadFile failed and the bridge could not migrate its DB on first boot.
//
// The loader had no test at all, which is why the windows-latest leg of
// tests.yml (CGO + sqlcipher) never caught it. Both tests below run on that
// leg. They pass unchanged on macOS/Linux, where filepath.Join and path.Join
// are identical, so the Windows leg is where they earn their keep.

// TestEmbeddedMigrationsResolve reads every embedded migration through the
// same forward-slash key construction the loader uses. If a *.sql file is
// added but not embedded (or renamed), or the embed directive changes, this
// fails on every platform.
func TestEmbeddedMigrationsResolve(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		key := path.Join("migrations", e.Name())
		b, err := migrationsFS.ReadFile(key)
		if err != nil {
			t.Fatalf("embedded migration %q unreadable at key %q: %v", e.Name(), key, err)
		}
		if len(b) == 0 {
			t.Fatalf("embedded migration %q is empty", e.Name())
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no embedded migrations found; embed directive or filenames changed")
	}
}

// TestApplyMigrationsFreshDB runs the real loader end-to-end against a fresh
// unencrypted SQLite DB (sqlcipher driver, no key — matches the EncryptDB=false
// production path). It exercises the exact ReadFile call site the Windows bug
// lived in, so a revert to filepath.Join fails this test on the windows leg.
func TestApplyMigrationsFreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // mirror production: SQLite is single-writer.

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations on fresh db: %v", err)
	}
	// Re-running must be a no-op: 001 is idempotent (IF NOT EXISTS / OR IGNORE)
	// and 002/003 are skipped via schema_version. A second run that errors would
	// mean the non-idempotent ALTER TABLE in 002 ran twice.
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations second run (idempotency): %v", err)
	}
	// Proof the migrations actually executed, not just that files were read:
	// the contacts table from 001 must exist.
	var name string
	if err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='contacts'`,
	).Scan(&name); err != nil {
		t.Fatalf("expected 'contacts' table after migrations: %v", err)
	}
}
