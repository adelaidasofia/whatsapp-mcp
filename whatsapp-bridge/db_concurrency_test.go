package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// MYC-3698 — /healthcheck was gated by DB contention, not query cost.
//
// The counters were already a covering index scan at 5.7ms and every query on
// the endpoint totalled ~0.48s, yet the endpoint took 4-27s on the live bridge.
// The cause was SetMaxOpenConns(1): every statement queued for the one
// connection, so an operator's health probe waited out whatever ingest was
// doing. WAL is the companion that makes widening the pool actually pay —
// readers stop contending with each committing writer — and a pinned
// busy_timeout keeps the now-genuinely-concurrent writers waiting rather than
// erroring.
//
// These tests assert CONFIGURATION and ORDERING, never wall-clock. A timing
// threshold would be flaky in CI and, worse, would pass on an idle machine
// regardless of the fix — which is exactly how this bottleneck hid behind
// MYC-3577 in the first place:
//
//	1. WAL is actually on, and persists (it is a database header property)
//	2. busy_timeout reaches EVERY pooled connection, not just the first
//	3. concurrent writers + readers produce ZERO errors under the shipped config
//	4. a short read overtakes a long one — with the negative control proving
//	   that a single-connection pool forces it to finish second

func openTestBridgeDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := &Config{
		DBPath:    filepath.Join(t.TempDir(), "concurrency.db"),
		EncryptDB: false, // WAL + pooling are orthogonal to encryption
	}
	db, err := OpenDB(cfg, "")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Property 1. OpenDB leaves the database in WAL, and it survives a reopen —
// proving the "set once, persisted in the header" claim the code relies on
// rather than assuming it.
func TestOpenDBEnablesWALAndItPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBPath: filepath.Join(dir, "persist.db"), EncryptDB: false}

	db, err := OpenDB(cfg, "")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if got := JournalMode(db); !strings.EqualFold(got, "wal") {
		t.Fatalf("journal_mode after OpenDB: want wal, got %q", got)
	}
	_ = db.Close()

	// Reopen with a plain driver connection that sets NOTHING. If WAL were a
	// per-connection setting rather than a header property, this would read
	// back "delete" and the whole design would be wrong.
	raw, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer raw.Close()
	var mode string
	if err := raw.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("WAL did not persist across reopen: got %q", mode)
	}
}

// Property 2. busy_timeout is per-CONNECTION. It rides in the DSN so the driver
// applies it to every connection it opens; an Exec would have configured
// exactly one and left the rest at 0. This forces several connections to be
// open simultaneously and checks each one.
func TestBusyTimeoutReachesEveryPooledConnection(t *testing.T) {
	db := openTestBridgeDB(t)

	const probes = 4
	// Hold several connections open at once so the pool is forced to create
	// distinct ones; checking sequentially could reuse a single connection and
	// pass even if only the first were configured.
	conns := make([]*sql.Conn, 0, probes)
	for i := 0; i < probes; i++ {
		c, err := db.Conn(t.Context())
		if err != nil {
			t.Fatalf("open conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var timeout int
		if err := c.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			t.Fatalf("read busy_timeout on conn %d: %v", i, err)
		}
		if timeout != dbBusyTimeoutMS {
			t.Fatalf("conn %d has busy_timeout=%d, want %d — the setting is not reaching every pooled connection", i, timeout, dbBusyTimeoutMS)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// concurrentWorkload runs writers and readers against db at the same time and
// returns every error it saw. Shared by the real test and its negative control
// so the two differ ONLY in configuration, never in what they do.
func concurrentWorkload(db *sql.DB, writers, readers, iterations int) []error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, err := db.Exec(
					`INSERT INTO messages (id, chat_jid, sender_jid, timestamp, type, content_text, is_from_me)
					 VALUES (?, ?, 's@lid', ?, 'text', 'hello', 0)
					 ON CONFLICT(id) DO NOTHING`,
					fmt.Sprintf("w%d-%d", w, i), "c@g.us", 1784846600+i)
				record(err)
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				var n int
				// The shape /healthcheck actually runs.
				record(db.QueryRow(
					`SELECT COUNT(*) FROM messages WHERE type = 'system'`).Scan(&n))
			}
		}()
	}
	wg.Wait()
	return errs
}

// Property 3. Under the shipped configuration, concurrent writers and readers
// complete with no errors at all. This is the claim the ticket makes.
func TestConcurrentWritersAndReadersProduceNoErrors(t *testing.T) {
	db := openTestBridgeDB(t)
	if _, err := db.Exec(
		`INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES ('c@g.us','group',0,0)`,
	); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	errs := concurrentWorkload(db, 4, 4, 40)
	if len(errs) != 0 {
		t.Fatalf("expected zero errors under WAL + busy_timeout, got %d; first: %v", len(errs), errs[0])
	}
}

// Property 4, the negative control — and the actual MECHANISM.
//
// Two earlier drafts of this control were wrong, and both were caught by the
// control failing rather than by inspection. Recording them because each one
// corrects a claim on MYC-3698:
//
//  1. "widen the pool without a busy timeout and writes error out" — came back
//     clean. The go-sqlcipher driver already DEFAULTS busy_timeout to 5000ms
//     (busyTimeout := 5000 in its source, confirmed by probing a fresh
//     connection). The `busy_timeout=0` on the ticket was read off the
//     `sqlcipher` CLI, whose session default is 0. Wrong surface for a claim
//     about a Go program.
//  2. "in rollback-journal mode an open write blocks readers" — also came back
//     clean. A small uncommitted write holds only a RESERVED lock, and RESERVED
//     still admits readers; blocking happens at COMMIT or on a cache spill, not
//     for the duration of the transaction.
//
// The property that actually produced the measured 4-27s is simpler and lives
// in Go, not SQLite: with SetMaxOpenConns(1) every statement queues for the one
// connection, so an unrelated /healthcheck read waits out whatever ingest is
// doing. This asserts that by ORDERING rather than by wall-clock, which is what
// keeps it out of the flaky-timing trap: with a pool of 1 the short query
// cannot even begin until the long one releases the connection, so it must
// finish second. With a wider pool it finishes first.
func completionOrderUnderPool(t *testing.T, maxOpen int) (shortFinishedFirst bool) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	var (
		mu    sync.Mutex
		order []string
		wg    sync.WaitGroup
	)
	finish := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	// Reserve a connection EXPLICITLY rather than racing for one. With
	// maxOpen=1 this is the only connection, so the short read below provably
	// cannot start until it is returned. An earlier draft spun on db.Stats()
	// instead and the short read simply won the race, which made the control
	// pass for the wrong reason.
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("reserve conn: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = conn.Close() }()
		// A deliberately slow, purely in-memory query, standing in for the
		// ingest work that was holding the single connection.
		var n int
		if err := conn.QueryRowContext(t.Context(), `WITH RECURSIVE c(x) AS (
			SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 2000000
		) SELECT count(*) FROM c`).Scan(&n); err != nil {
			t.Errorf("long query: %v", err)
		}
		finish("long")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var one int
		if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			t.Errorf("short query: %v", err)
		}
		finish("short")
	}()

	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return len(order) > 0 && order[0] == "short"
}

// The shipped configuration: a trivial read is NOT stuck behind a long-running
// one. This is what lets /healthcheck answer while ingest is working.
func TestWiderPoolLetsAShortReadOvertakeALongOne(t *testing.T) {
	if !completionOrderUnderPool(t, dbMaxOpenConns) {
		t.Fatalf("with a pool of %d a short read should not wait for a long one to finish", dbMaxOpenConns)
	}
}

// The negative control: the OLD configuration, SetMaxOpenConns(1). The short
// read is forced to finish second because it cannot get a connection. If this
// ever passes, the pool size is not what was causing the queueing and the test
// above proves nothing.
func TestNegativeControl_SingleConnectionPoolSerializesUnrelatedReads(t *testing.T) {
	if completionOrderUnderPool(t, 1) {
		t.Fatal("NEGATIVE CONTROL FAILED: a short read overtook a long one with SetMaxOpenConns(1), so TestWiderPoolLetsAShortReadOvertakeALongOne proves nothing about the pool size")
	}
}
