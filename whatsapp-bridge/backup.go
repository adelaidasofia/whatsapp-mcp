package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupRunner takes periodic snapshots of the message store.
//
// WHATSAPP_BACKUP_PATH has been documented in .env.example since the first
// release and never did anything: Config.BackupPath was declared, assigned from
// the environment, and read by nothing. Go rejects an unused local variable but
// says nothing about an unused struct FIELD, so the dead knob compiled cleanly
// while operators reasonably believed their store was being backed up. This is
// the code that makes the setting true.
//
// Snapshots use SQLite's VACUUM INTO rather than copying files. That matters
// for three reasons:
//
//   - It is consistent on a LIVE database. A file copy has to reason about the
//     -wal, which routinely holds megabytes of committed-but-uncheckpointed
//     pages; copying messages.db alone loses exactly the recent traffic you
//     would most want back, and copying db+wal+shm together while writes land
//     is not atomic. VACUUM INTO reads a single consistent view.
//   - It needs no downtime. An external backup script has to stop the bridge,
//     because it cannot decrypt the store. This runs inside the process that
//     already holds the key and the handle.
//   - On SQLCipher it inherits the source's encryption. The snapshot carries
//     the same cipher header and reopens with the same key, so a backup is
//     never a plaintext copy of every message sitting beside the encrypted one.
//     That is not an assumption; TestSnapshotOfEncryptedDBIsAlsoEncrypted
//     asserts it against a real canary.
type BackupRunner struct {
	db       *sql.DB
	dir      string
	interval time.Duration
	keep     int
}

// backupPrefix and backupTimeLayout make snapshot names sort chronologically as
// plain strings, so retention never has to stat or parse to find the newest.
const (
	backupPrefix     = "messages-"
	backupSuffix     = ".db"
	backupTimeLayout = "20060102-150405"
	backupTempPrefix = ".tmp-"
)

// NewBackupRunner returns nil when backups are disabled (interval <= 0 or no
// directory configured), so the caller can treat "disabled" and "not running"
// identically.
func NewBackupRunner(db *sql.DB, dir string, intervalHours, keep int) *BackupRunner {
	if dir == "" || intervalHours <= 0 {
		return nil
	}
	if keep < 1 {
		keep = 1
	}
	return &BackupRunner{
		db:       db,
		dir:      dir,
		interval: time.Duration(intervalHours) * time.Hour,
		keep:     keep,
	}
}

// Run takes a snapshot on startup when one is due, then every interval.
//
// The startup snapshot is conditional on purpose. Running unconditionally means
// a process that restarts often backs up on every boot; running only on the
// ticker means a process that restarts more often than the interval never backs
// up at all. Asking whether the newest existing snapshot is older than the
// interval gets both right, and is self-limiting under a crash loop.
func (b *BackupRunner) Run(ctx context.Context) {
	if b == nil {
		return
	}

	due, err := b.dueAtStartup()
	if err != nil {
		log.Printf("backup: cannot inspect %s: %v", b.dir, err)
	} else if due {
		// Small delay so a snapshot never competes with connection setup.
		select {
		case <-time.After(2 * time.Minute):
			b.runOnce(ctx)
		case <-ctx.Done():
			return
		}
	}

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.runOnce(ctx)
		}
	}
}

func (b *BackupRunner) runOnce(ctx context.Context) {
	path, err := b.Snapshot(ctx)
	if err != nil {
		// Non-fatal by design: a failed backup must never take down a working
		// bridge. It is logged loudly so a monitor can see it.
		log.Printf("backup FAILED: %v", err)
		return
	}
	size := int64(0)
	if fi, statErr := os.Stat(path); statErr == nil {
		size = fi.Size()
	}
	log.Printf("backup: wrote %s (%.1f MB)", filepath.Base(path), float64(size)/(1024*1024))

	removed, err := b.Prune()
	if err != nil {
		log.Printf("backup: prune failed: %v", err)
	} else if removed > 0 {
		log.Printf("backup: pruned %d old snapshot(s), keeping %d", removed, b.keep)
	}
}

// Snapshot writes one consistent snapshot and returns its path.
//
// It writes to a dotted temporary name and renames only on success. A snapshot
// interrupted midway (crash, full disk, killed process) therefore leaves a
// .tmp- file that Prune and any restore logic ignore, rather than a truncated
// messages-*.db that looks exactly like a good one. Retention that trusts a
// half-written snapshot is how a backup directory stays reassuringly full while
// holding nothing restorable.
func (b *BackupRunner) Snapshot(ctx context.Context) (string, error) {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	stamp := time.Now().Format(backupTimeLayout)
	tmp := filepath.Join(b.dir, backupTempPrefix+stamp+backupSuffix)
	final := filepath.Join(b.dir, backupPrefix+stamp+backupSuffix)

	// A stale temp from a previous crash would make VACUUM INTO fail: it
	// refuses to write a file that already exists.
	_ = os.Remove(tmp)

	if _, err := b.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("vacuum into %s: %w", tmp, err)
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		return "", fmt.Errorf("stat snapshot: %w", err)
	}
	// A SQLite/SQLCipher file is at minimum one page. Anything smaller is not a
	// database, and must not be renamed into place where it would be mistaken
	// for one.
	if fi.Size() < 512 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("snapshot is only %d bytes; discarded", fi.Size())
	}

	// Best effort: the snapshot holds the same messages as the store, so it
	// deserves the same permissions. A no-op on Windows.
	_ = os.Chmod(tmp, 0o600)

	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("publish snapshot: %w", err)
	}
	return final, nil
}

// Prune keeps the newest `keep` snapshots and deletes the rest. It also clears
// temporary files left by interrupted runs.
func (b *BackupRunner) Prune() (int, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var snapshots []string
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, backupTempPrefix) {
			if os.Remove(filepath.Join(b.dir, name)) == nil {
				removed++
			}
			continue
		}
		if strings.HasPrefix(name, backupPrefix) && strings.HasSuffix(name, backupSuffix) {
			snapshots = append(snapshots, name)
		}
	}

	// Names embed a fixed-width timestamp, so lexical order IS chronological.
	sort.Sort(sort.Reverse(sort.StringSlice(snapshots)))
	for i, name := range snapshots {
		if i < b.keep {
			continue
		}
		if err := os.Remove(filepath.Join(b.dir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// dueAtStartup reports whether the newest snapshot is older than one interval
// (or absent entirely).
func (b *BackupRunner) dueAtStartup() (bool, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}

	var newest time.Time
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupSuffix) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, backupPrefix), backupSuffix)
		when, parseErr := time.ParseInLocation(backupTimeLayout, stamp, time.Local)
		if parseErr != nil {
			continue
		}
		if when.After(newest) {
			newest = when
		}
	}

	if newest.IsZero() {
		return true, nil
	}
	return time.Since(newest) >= b.interval, nil
}
