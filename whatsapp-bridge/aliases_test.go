package main

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	_ "github.com/mutecomm/go-sqlcipher/v4"
	"go.mau.fi/whatsmeow/types"
)

// --- Setup helper -----------------------------------------------------------

// newAliasTestDB returns an in-memory SQLite DB with the jid_aliases schema
// applied. No SQLCipher key set, so the driver behaves as plain SQLite.
// Each test gets its own clean DB so tests don't pollute each other.
func newAliasTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Minimal schema: just the jid_aliases table from 003_jid_aliases.sql.
	// We don't need the full migration chain for these tests.
	_, err = db.Exec(`
		CREATE TABLE jid_aliases (
			jid_a TEXT NOT NULL,
			jid_b TEXT NOT NULL,
			discovered_at INTEGER NOT NULL,
			source TEXT NOT NULL,
			PRIMARY KEY (jid_a, jid_b)
		);
		CREATE INDEX idx_jid_aliases_b ON jid_aliases(jid_b);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Fictional JID user-portions for tests. Using the US 555-reserved range for
// the phone form, opaque hex for the LID form. No real number referenced.
const (
	testPhoneUser = "15555550100"
	testPhoneUser2 = "15555550101"
	testLIDUser  = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	testLIDUser2 = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// --- Pure helper tests ------------------------------------------------------

// TestInClausePlaceholders verifies the SQL placeholder builder used by
// list_messages when merging multiple JIDs for the same contact. Correctness
// here is what makes parameterised IN-clauses safe; a wrong placeholder count
// silently breaks the merge or causes a SQL error.
func TestInClausePlaceholders(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{-1, ""},
		{1, "?"},
		{2, "?,?"},
		{3, "?,?,?"},
		{5, "?,?,?,?,?"},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			got := inClausePlaceholders(tc.n)
			if got != tc.want {
				t.Errorf("inClausePlaceholders(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

// TestJidsToArgs verifies the []string → []any widening that lets us pass a
// JID slice into sql.QueryContext's varargs without manual loops at every
// call site.
func TestJidsToArgs(t *testing.T) {
	cases := []struct {
		in   []string
		want []any
	}{
		{nil, []any{}},
		{[]string{}, []any{}},
		{[]string{"x@s.whatsapp.net"}, []any{"x@s.whatsapp.net"}},
		{[]string{"a@lid", "b@s.whatsapp.net", "c@lid"}, []any{"a@lid", "b@s.whatsapp.net", "c@lid"}},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			got := jidsToArgs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("jidsToArgs(%v) length = %d, want %d", tc.in, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("jidsToArgs(%v)[%d] = %v, want %v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- recordJIDAlias tests ---------------------------------------------------

// TestRecordJIDAliasInsertsBothDirections is the core correctness check.
// One call should yield two rows in jid_aliases so a subsequent PK lookup
// resolves either direction in one shot.
func TestRecordJIDAliasInsertsBothDirections(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	phoneJID := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	lidJID := types.JID{User: testLIDUser, Server: types.HiddenUserServer}

	recordJIDAlias(ctx, db, phoneJID, lidJID, "message_event", 1700000000)

	if got := countRows(t, db, "jid_aliases"); got != 2 {
		t.Errorf("expected 2 rows after one recordJIDAlias call, got %d", got)
	}

	// Both directions must exist.
	var sourceA, sourceB string
	err := db.QueryRowContext(ctx,
		`SELECT source FROM jid_aliases WHERE jid_a = ? AND jid_b = ?`,
		phoneJID.String(), lidJID.String(),
	).Scan(&sourceA)
	if err != nil {
		t.Errorf("forward edge missing: %v", err)
	}
	err = db.QueryRowContext(ctx,
		`SELECT source FROM jid_aliases WHERE jid_a = ? AND jid_b = ?`,
		lidJID.String(), phoneJID.String(),
	).Scan(&sourceB)
	if err != nil {
		t.Errorf("reverse edge missing: %v", err)
	}
	if sourceA != "message_event" || sourceB != "message_event" {
		t.Errorf("source field not preserved: forward=%q reverse=%q", sourceA, sourceB)
	}
}

// TestRecordJIDAliasIdempotent verifies the PRIMARY KEY conflict path: calling
// recordJIDAlias twice with the same pair does not duplicate rows.
func TestRecordJIDAliasIdempotent(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	a := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	b := types.JID{User: testLIDUser, Server: types.HiddenUserServer}

	recordJIDAlias(ctx, db, a, b, "first_call", 1700000000)
	recordJIDAlias(ctx, db, a, b, "second_call", 1700000100)

	if got := countRows(t, db, "jid_aliases"); got != 2 {
		t.Errorf("expected 2 rows after duplicate recordJIDAlias, got %d", got)
	}
}

// TestRecordJIDAliasEmptyJIDsAreNoOp verifies the guard clauses that protect
// against accidentally writing rows when whatsmeow surfaces an empty JID
// (e.g. an event with no SenderAlt).
func TestRecordJIDAliasEmptyJIDsAreNoOp(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	valid := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	empty := types.JID{}

	recordJIDAlias(ctx, db, valid, empty, "test", 1700000000)
	recordJIDAlias(ctx, db, empty, valid, "test", 1700000000)
	recordJIDAlias(ctx, db, empty, empty, "test", 1700000000)

	if got := countRows(t, db, "jid_aliases"); got != 0 {
		t.Errorf("expected 0 rows from all-empty recordJIDAlias calls, got %d", got)
	}
}

// TestRecordJIDAliasSameJIDIsNoOp verifies the guard against self-edges.
// A.String() == B.String() means it's the same JID, no edge to record.
func TestRecordJIDAliasSameJIDIsNoOp(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	jid := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}

	recordJIDAlias(ctx, db, jid, jid, "self", 1700000000)

	if got := countRows(t, db, "jid_aliases"); got != 0 {
		t.Errorf("expected 0 rows for self-alias, got %d", got)
	}
}

// --- resolveAliases tests ---------------------------------------------------

// TestResolveAliasesEmptyTable verifies the no-aliases-known case: the function
// must still return the queried JID as the first (and only) element so
// downstream code never gets an empty slice for a valid input.
func TestResolveAliasesEmptyTable(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	jid := testPhoneUser + "@s.whatsapp.net"
	got, err := resolveAliases(ctx, db, jid)
	if err != nil {
		t.Fatalf("resolveAliases: %v", err)
	}
	if len(got) != 1 || got[0] != jid {
		t.Errorf("expected [%q], got %v", jid, got)
	}
}

// TestResolveAliasesEmptyJIDReturnsNil verifies the empty-input early exit.
// Important: an empty JID must not query the DB at all, since that would
// match every row that happens to have an empty jid_a (impossible by schema,
// but defensive).
func TestResolveAliasesEmptyJIDReturnsNil(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	got, err := resolveAliases(ctx, db, "")
	if err != nil {
		t.Fatalf("resolveAliases: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty jid, got %v", got)
	}
}

// TestResolveAliasesIncludesQueryFirst verifies the order contract:
// the queried JID always appears as out[0], with aliases following. Callers
// rely on this to build IN-clauses where the original JID is the canonical
// one for display.
func TestResolveAliasesIncludesQueryFirst(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	phone := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	lid := types.JID{User: testLIDUser, Server: types.HiddenUserServer}
	recordJIDAlias(ctx, db, phone, lid, "test", 1700000000)

	got, err := resolveAliases(ctx, db, phone.String())
	if err != nil {
		t.Fatalf("resolveAliases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d (%v)", len(got), got)
	}
	if got[0] != phone.String() {
		t.Errorf("queried JID must be at index 0, got %v", got)
	}
	if got[1] != lid.String() {
		t.Errorf("alias must be at index 1, got %v", got)
	}
}

// TestResolveAliasesMultipleAliases covers the case where a single human has
// MORE than one alias (e.g. two LID rotations against one phone). All should
// surface together.
func TestResolveAliasesMultipleAliases(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	phone := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	lid1 := types.JID{User: testLIDUser, Server: types.HiddenUserServer}
	lid2 := types.JID{User: testLIDUser2, Server: types.HiddenUserServer}

	recordJIDAlias(ctx, db, phone, lid1, "ev1", 1700000000)
	recordJIDAlias(ctx, db, phone, lid2, "ev2", 1700000100)

	got, err := resolveAliases(ctx, db, phone.String())
	if err != nil {
		t.Fatalf("resolveAliases: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results (phone + 2 lids), got %d (%v)", len(got), got)
	}
	// Phone must be first; the two LIDs follow in some order.
	if got[0] != phone.String() {
		t.Errorf("queried JID must be at index 0, got %v", got)
	}
	tail := []string{got[1], got[2]}
	sort.Strings(tail)
	wantTail := []string{lid1.String(), lid2.String()}
	sort.Strings(wantTail)
	for i := range tail {
		if tail[i] != wantTail[i] {
			t.Errorf("alias mismatch at %d: got %q, want %q", i, tail[i], wantTail[i])
		}
	}
}

// TestResolveAliasesReverseDirection verifies the symmetric edge property:
// querying with the LID returns the phone as alias, not just the other way.
// This is the property that broke the original LID/phone JID split bug (see
// migrations/003_jid_aliases.sql for the regression background).
func TestResolveAliasesReverseDirection(t *testing.T) {
	db := newAliasTestDB(t)
	ctx := context.Background()

	phone := types.JID{User: testPhoneUser, Server: types.DefaultUserServer}
	lid := types.JID{User: testLIDUser, Server: types.HiddenUserServer}
	recordJIDAlias(ctx, db, phone, lid, "test", 1700000000)

	// Query with the LID this time, not the phone.
	got, err := resolveAliases(ctx, db, lid.String())
	if err != nil {
		t.Fatalf("resolveAliases: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d (%v)", len(got), got)
	}
	if got[0] != lid.String() {
		t.Errorf("queried JID (lid) must be at index 0, got %v", got)
	}
	if got[1] != phone.String() {
		t.Errorf("alias (phone) must be at index 1, got %v", got)
	}
}
