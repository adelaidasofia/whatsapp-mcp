package main

import (
	"database/sql"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Coverage for the calls table, which never held a single row.
//
// The bug. onCallOffer INSERTs result='offered', but the column was declared
//
//	result TEXT CHECK (result IN ('answered','missed','rejected','ended','failed'))
//
// and 'offered' is not in that list, so EVERY offer failed the CHECK. The error
// was logged and discarded, so the process looked healthy while capture_calls
// recorded nothing. Measured on a live bridge (v0.4.1, 2026-08-26): 13 straight
// `onCallOffer: insert failed: CHECK constraint failed: calls` in one startup,
// zero rows written.
//
// The second defect only became reachable once the first was fixed, which is
// why this file tests the terminate path separately. onCallTerminate wrote
// strings.ToLower(evt.Reason) straight into the same CHECKed column, and Reason
// is not an enum — whatsmeow lifts it verbatim off the wire as
// cag.String("reason") (call.go:92). WhatsApp chooses that string. 'timeout'
// and 'decline' are both real values and neither is in the list, so the UPDATE
// would have failed the CHECK too. It never got the chance: with the INSERT
// failing there was no row to match, so `UPDATE ... WHERE id = ?` touched 0
// rows and returned no error. One bug hid the other.
//
// The tests below drive the real event handlers rather than hand-written SQL.
// A test that INSERTs its own row would prove the schema accepts what the test
// chose to write, which is not the question.

func callsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "calls_test.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db
}

func callsTestBridge(t *testing.T) (*Bridge, *sql.DB) {
	t.Helper()
	db := callsTestDB(t)
	return &Bridge{cfg: &Config{CaptureCalls: true}, db: db}, db
}

func callsTestJID(t *testing.T, s string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(s)
	if err != nil {
		t.Fatalf("parse jid %q: %v", s, err)
	}
	return jid
}

func offerEvent(t *testing.T, callID string) *events.CallOffer {
	t.Helper()
	return &events.CallOffer{
		BasicCallMeta: types.BasicCallMeta{
			From:        callsTestJID(t, "573001112233@s.whatsapp.net"),
			CallCreator: callsTestJID(t, "573001112233@s.whatsapp.net"),
			CallID:      callID,
			Timestamp:   time.Unix(1787750000, 0),
		},
	}
}

// callRow reads back what the handlers actually stored.
func callRow(t *testing.T, db *sql.DB, callID string) (result string, rawResult sql.NullString, found bool) {
	t.Helper()
	err := db.QueryRow(`SELECT result, result_raw FROM calls WHERE id = ?`, callID).Scan(&result, &rawResult)
	if err == sql.ErrNoRows {
		return "", sql.NullString{}, false
	}
	if err != nil {
		t.Fatalf("read call %s: %v", callID, err)
	}
	return result, rawResult, true
}

// TestCallOfferIsRecorded is the direct regression guard: before the fix this
// wrote nothing at all.
func TestCallOfferIsRecorded(t *testing.T) {
	b, db := callsTestBridge(t)

	b.onCallOffer(offerEvent(t, "CALL-OFFER-1"))

	result, _, found := callRow(t, db, "CALL-OFFER-1")
	if !found {
		t.Fatal("call offer wrote no row: the CHECK constraint rejected it and the error was only logged")
	}
	if result != callResultOffered {
		t.Fatalf("result = %q, want %q", result, callResultOffered)
	}
}

// TestCallTerminateNormalizesWireReasons pins the mapping from WhatsApp's
// wire-chosen reason to the closed vocabulary the column stores. These reason
// strings are the ones WhatsApp actually sends; none of them were storable
// before.
func TestCallTerminateNormalizesWireReasons(t *testing.T) {
	cases := []struct {
		wireReason string
		want       string
	}{
		{"timeout", callResultMissed},
		{"decline", callResultRejected},
		{"reject", callResultRejected},
		{"Decline", callResultRejected}, // case from the wire is not ours to trust
		{"accept", callResultAnswered},
		{"hangup", callResultEnded},
		{"connection-lost", callResultFailed},
		{"", callResultEnded}, // preserved from the original handler
	}

	for _, tc := range cases {
		t.Run(tc.wireReason, func(t *testing.T) {
			b, db := callsTestBridge(t)
			callID := "CALL-" + tc.wireReason
			b.onCallOffer(offerEvent(t, callID))

			b.onCallTerminate(&events.CallTerminate{
				BasicCallMeta: types.BasicCallMeta{CallID: callID},
				Reason:        tc.wireReason,
			})

			result, _, found := callRow(t, db, callID)
			if !found {
				t.Fatalf("row vanished for reason %q", tc.wireReason)
			}
			if result != tc.want {
				t.Fatalf("reason %q stored as %q, want %q", tc.wireReason, result, tc.want)
			}
		})
	}
}

// TestUnknownWireReasonIsKeptNotDropped is the structural half of the fix.
//
// A closed CHECK over a value the REMOTE party chooses is the shape of the
// original defect: any reason WhatsApp adds later re-breaks the write. An
// unrecognized reason must therefore still produce a stored row — classified
// as unknown, with the wire string preserved verbatim so the mapping can be
// extended from real data instead of guesses.
func TestUnknownWireReasonIsKeptNotDropped(t *testing.T) {
	b, db := callsTestBridge(t)
	b.onCallOffer(offerEvent(t, "CALL-FUTURE"))

	b.onCallTerminate(&events.CallTerminate{
		BasicCallMeta: types.BasicCallMeta{CallID: "CALL-FUTURE"},
		Reason:        "some_reason_whatsapp_invents_in_2027",
	})

	result, raw, found := callRow(t, db, "CALL-FUTURE")
	if !found {
		t.Fatal("an unrecognized reason dropped the call row entirely")
	}
	if result != callResultUnknown {
		t.Fatalf("result = %q, want %q", result, callResultUnknown)
	}
	if raw.String != "some_reason_whatsapp_invents_in_2027" {
		t.Fatalf("result_raw = %q, want the verbatim wire reason", raw.String)
	}
}

// TestCaptureCallsOffRecordsNothing guards the config switch, so the fix does
// not quietly start writing for users who turned capture off.
func TestCaptureCallsOffRecordsNothing(t *testing.T) {
	db := callsTestDB(t)
	b := &Bridge{cfg: &Config{CaptureCalls: false}, db: db}

	b.onCallOffer(offerEvent(t, "CALL-OFF"))

	if _, _, found := callRow(t, db, "CALL-OFF"); found {
		t.Fatal("capture_calls=false still wrote a call row")
	}
}

// TestMigration007UpgradesAnExistingStore covers the path every existing
// install takes, which running applyMigrations against an empty file does not.
//
// 007 rebuilds the table: CREATE, copy, DROP, RENAME. On a fresh database that
// copy moves nothing and the DROP costs nothing, so a from-scratch test proves
// only that the new schema parses. The risk lives entirely in the upgrade —
// rows must survive the copy, and DROP TABLE silently takes the indexes with
// it, so a migration that forgot to recreate them would leave every install
// with an unindexed calls table and no failing test anywhere.
func TestMigration007UpgradesAnExistingStore(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	// Bring the store to the pre-007 state the way a real install got there.
	applyMigrationsUpTo(t, db, 6)

	// A row that was legal under the OLD CHECK. The live bug means production
	// stores are empty, but the migration must not depend on that.
	if _, err := db.Exec(`INSERT INTO calls (id, chat_jid, caller_jid, timestamp, call_type, is_group, is_outbound, result)
	                      VALUES ('LEGACY-1', 'x@s.whatsapp.net', 'x@s.whatsapp.net', 1787000000, 'video', 0, 1, 'ended')`); err != nil {
		t.Fatalf("seed pre-007 row: %v", err)
	}

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations to head: %v", err)
	}

	// 1. The pre-existing row survived the rebuild, with its values intact.
	var callType, result string
	var isOutbound int
	var rawResult sql.NullString
	if err := db.QueryRow(`SELECT call_type, is_outbound, result, result_raw FROM calls WHERE id = 'LEGACY-1'`).
		Scan(&callType, &isOutbound, &result, &rawResult); err != nil {
		t.Fatalf("pre-007 row did not survive the rebuild: %v", err)
	}
	if callType != "video" || isOutbound != 1 || result != callResultEnded {
		t.Fatalf("row corrupted by the copy: call_type=%q is_outbound=%d result=%q", callType, isOutbound, result)
	}
	if rawResult.Valid {
		t.Fatalf("result_raw should be NULL for a migrated row, got %q", rawResult.String)
	}

	// 2. The widened vocabulary is actually in effect.
	if _, err := db.Exec(`INSERT INTO calls (id, chat_jid, caller_jid, timestamp, call_type, is_group, is_outbound, result)
	                      VALUES ('NEW-1', 'x@s.whatsapp.net', 'x@s.whatsapp.net', 1787000001, 'voice', 0, 0, ?)`,
		callResultOffered); err != nil {
		t.Fatalf("post-migration insert of %q still rejected: %v", callResultOffered, err)
	}

	// 3. The CHECK still rejects what is outside the vocabulary. A migration
	//    that dropped the constraint instead of widening it would pass every
	//    other assertion here.
	if _, err := db.Exec(`INSERT INTO calls (id, chat_jid, caller_jid, timestamp, call_type, is_group, is_outbound, result)
	                      VALUES ('BAD-1', 'x@s.whatsapp.net', 'x@s.whatsapp.net', 1787000002, 'voice', 0, 0, 'not_a_result')`); err == nil {
		t.Fatal("the CHECK was dropped, not widened: an arbitrary result was accepted")
	}

	// 4. DROP TABLE took the indexes; 007 must have put them back.
	for _, idx := range []string{"idx_calls_chat_time", "idx_calls_caller_time"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Fatalf("index %s missing after the rebuild: %v", idx, err)
		}
	}
}

// applyMigrationsUpTo runs the embedded migrations through version max, so a
// test can stand a store up in a historical state. It deliberately reuses the
// real embedded files rather than a copy of their DDL: a hand-written snapshot
// of the old schema would drift from what installs actually have.
func applyMigrationsUpTo(t *testing.T, db *sql.DB, max int) {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if parseMigrationVersion(name) > max {
			continue
		}
		b, err := migrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}
