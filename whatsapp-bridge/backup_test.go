package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

func backupTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db, filepath.Join(dir, "backups")
}

func snapshotNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// The snapshot has to be a real, reopenable database that actually contains the
// rows -- not merely a file that exists.
func TestSnapshotIsAReadableDatabase(t *testing.T) {
	db, dir := backupTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES (?, 'direct', 1, 1)`,
		"1555@s.whatsapp.net"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := NewBackupRunner(db, dir, 24, 7)
	path, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	snap, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()

	var jid string
	if err := snap.QueryRow(`SELECT jid FROM chats`).Scan(&jid); err != nil {
		t.Fatalf("snapshot is not a usable database: %v", err)
	}
	if jid != "1555@s.whatsapp.net" {
		t.Errorf("jid = %q, want the seeded row", jid)
	}
}

// The one that matters most. A backup of an ENCRYPTED store must itself be
// encrypted, or enabling backups quietly writes every message in the clear
// beside the store that was carefully encrypted. Asserted with a canary rather
// than trusted from documentation.
func TestSnapshotOfEncryptedDBIsAlsoEncrypted(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "enc.db")
	backups := filepath.Join(dir, "backups")
	key := strings.Repeat("ab", 32)

	dsn := fmt.Sprintf("%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", src, key)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open encrypted: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	const canary = "CANARY_PLAINTEXT_MARKER"
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, canary); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Guard the guard: if the SOURCE ever stops being encrypted, this test
	// would pass for the wrong reason.
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if strings.Contains(string(srcBytes), canary) {
		t.Fatal("source database is not encrypted; this test cannot prove anything")
	}

	r := NewBackupRunner(db, backups, 24, 7)
	path, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	snapBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(snapBytes), canary) {
		t.Fatal("LEAK: the snapshot of an encrypted store contains plaintext")
	}
	if strings.HasPrefix(string(snapBytes), "SQLite format 3") {
		t.Error("snapshot carries a plaintext SQLite header; it is not encrypted")
	}

	// And it must still be readable with the original key, or it is not a
	// backup, just an unreadable file.
	snapDSN := fmt.Sprintf("%s?_pragma_key=x'%s'&_pragma_cipher_page_size=4096", path, key)
	snap, err := sql.Open("sqlite3", snapDSN)
	if err != nil {
		t.Fatalf("reopen snapshot: %v", err)
	}
	defer snap.Close()
	var got string
	if err := snap.QueryRow(`SELECT v FROM t`).Scan(&got); err != nil {
		t.Fatalf("snapshot not readable with the source key: %v", err)
	}
	if got != canary {
		t.Errorf("got %q, want %q", got, canary)
	}
}

func TestPruneKeepsNewestAndDropsTheRest(t *testing.T) {
	db, dir := backupTestDB(t)
	r := NewBackupRunner(db, dir, 24, 3)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Fixed, ordered stamps: retention must not depend on filesystem mtimes.
	stamps := []string{
		"20260101-000000", "20260102-000000", "20260103-000000",
		"20260104-000000", "20260105-000000",
	}
	for _, s := range stamps {
		p := filepath.Join(dir, backupPrefix+s+backupSuffix)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	removed, err := r.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	got := snapshotNames(t, dir)
	if len(got) != 3 {
		t.Fatalf("kept %d snapshots, want 3: %v", len(got), got)
	}
	for _, want := range []string{"20260103-000000", "20260104-000000", "20260105-000000"} {
		found := false
		for _, g := range got {
			if strings.Contains(g, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %s to survive retention; kept %v", want, got)
		}
	}
}

// An interrupted snapshot leaves a .tmp- file. It must never be mistaken for a
// real one, and must be cleaned up rather than accumulating forever.
func TestPruneRemovesInterruptedTempFilesAndNeverCountsThem(t *testing.T) {
	db, dir := backupTestDB(t)
	r := NewBackupRunner(db, dir, 24, 3)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tmp := filepath.Join(dir, backupTempPrefix+"20260101-000000"+backupSuffix)
	if err := os.WriteFile(tmp, []byte("half written"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	real := filepath.Join(dir, backupPrefix+"20260102-000000"+backupSuffix)
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}

	if _, err := r.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("interrupted temp file survived prune")
	}
	if _, err := os.Stat(real); err != nil {
		t.Error("prune deleted a real snapshot while keep=3")
	}
}

// A crashed run can leave a temp file with the same stamp. The next snapshot in
// that second must not fail because VACUUM INTO refuses an existing target.
func TestSnapshotOverwritesAStaleTempFile(t *testing.T) {
	db, dir := backupTestDB(t)
	r := NewBackupRunner(db, dir, 24, 7)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stamp := time.Now().Format(backupTimeLayout)
	stale := filepath.Join(dir, backupTempPrefix+stamp+backupSuffix)
	if err := os.WriteFile(stale, []byte("garbage from a crashed run"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot did not recover from a stale temp file: %v", err)
	}
}

// Disabled must mean disabled, and must be expressible.
func TestBackupsCanBeDisabled(t *testing.T) {
	db, dir := backupTestDB(t)
	if r := NewBackupRunner(db, dir, 0, 7); r != nil {
		t.Error("interval 0 should disable backups")
	}
	if r := NewBackupRunner(db, "", 24, 7); r != nil {
		t.Error("empty backup dir should disable backups")
	}
	// A nil runner must be safe to Run, so callers need no nil check.
	var nilRunner *BackupRunner
	nilRunner.Run(context.Background())
}

// keep < 1 would delete every snapshot the moment it is written, turning
// retention into deletion.
func TestKeepIsClampedToAtLeastOne(t *testing.T) {
	db, dir := backupTestDB(t)
	r := NewBackupRunner(db, dir, 24, 0)
	if r == nil {
		t.Fatal("runner should exist")
	}
	if r.keep < 1 {
		t.Errorf("keep = %d, want >= 1", r.keep)
	}

	if _, err := r.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := r.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := snapshotNames(t, dir); len(got) != 1 {
		t.Errorf("kept %d snapshots, want 1 -- retention must never delete everything", len(got))
	}
}

func TestDueAtStartup(t *testing.T) {
	db, dir := backupTestDB(t)
	r := NewBackupRunner(db, dir, 24, 7)

	// No directory at all: a store that has never been backed up is due.
	due, err := r.dueAtStartup()
	if err != nil {
		t.Fatalf("dueAtStartup: %v", err)
	}
	if !due {
		t.Error("a store with no backups should be due")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A snapshot from moments ago: not due, so a restart loop cannot spam
	// snapshots.
	recent := time.Now().Add(-1 * time.Hour).Format(backupTimeLayout)
	if err := os.WriteFile(filepath.Join(dir, backupPrefix+recent+backupSuffix), []byte("x"), 0o600); err != nil {
		t.Fatalf("write recent: %v", err)
	}
	due, err = r.dueAtStartup()
	if err != nil {
		t.Fatalf("dueAtStartup: %v", err)
	}
	if due {
		t.Error("a snapshot 1h old should not be due with a 24h interval")
	}

	// One older than the interval: due again.
	old := time.Now().Add(-48 * time.Hour).Format(backupTimeLayout)
	if err := os.Remove(filepath.Join(dir, backupPrefix+recent+backupSuffix)); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupPrefix+old+backupSuffix), []byte("x"), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	due, err = r.dueAtStartup()
	if err != nil {
		t.Fatalf("dueAtStartup: %v", err)
	}
	if !due {
		t.Error("a snapshot 48h old should be due with a 24h interval")
	}
}
