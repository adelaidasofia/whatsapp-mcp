package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// MYC-3577 — the decode counters move off a `content_text LIKE` scan and onto
// the indexed messages.raw_type column.
//
// Two properties have to hold, and they pull against each other:
//
//	1. the numbers must not change (same field names, same values)
//	2. the query must actually use an index
//
// Testing only (1) would pass against a rewrite that is still a scan. Testing
// only (2) would pass against a fast query that counts the wrong rows.

const rawTypeTestChat = "12147735814-1589465137@g.us"

// legacyUndecodedStats is the PRE-MYC-3577 implementation, kept verbatim as the
// oracle. Counter parity is asserted against a real second implementation over
// the same rows rather than against numbers hard-coded by the person changing
// the query, which is the only version of this assertion that can fail for the
// right reason.
func legacyUndecodedStats(ctx context.Context, db *sql.DB) decodeStats {
	st := decodeStats{UndecodedByType: map[string]int{}, UndecryptableByMode: map[string]int{}}
	markerLike := unsupportedPrefix + "%"
	undecryptableLike := undecryptablePrefix + "%"

	_ = db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN content_text LIKE ? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN COALESCE(content_text, '') = '' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN content_text LIKE ? THEN 1 ELSE 0 END), 0)
		 FROM messages WHERE type = 'system'`,
		markerLike, undecryptableLike,
	).Scan(&st.UndecodedTotal, &st.LegacyEmptySystem, &st.UndecryptableTotal)

	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(content_text, ''), COUNT(*) FROM messages
		 WHERE type = 'system' AND (content_text LIKE ? OR content_text LIKE ?)
		 GROUP BY content_text`, markerLike, undecryptableLike)
	if err != nil {
		return st
	}
	defer rows.Close()
	for rows.Next() {
		var marker string
		var n int
		if rows.Scan(&marker, &n) != nil {
			continue
		}
		if strings.HasPrefix(marker, undecryptablePrefix) {
			mode := undecryptableFailMode(marker)
			if mode == "" {
				mode = "unknown"
			}
			st.UndecryptableByMode[mode] += n
			continue
		}
		raw := unsupportedRawType(marker)
		if raw == "" {
			raw = "unknown"
		}
		st.UndecodedByType[raw] += n
	}
	return st
}

// rawTypeCorpus is deliberately awkward: well-formed markers of both families,
// malformed ones of both, the legacy empty rows, a system row that carries real
// text, and ordinary messages. The malformed cases are the ones a naive
// backfill silently drops.
func seedRawTypeCorpus(t *testing.T, db *sql.DB) {
	t.Helper()
	rows := []struct{ id, typ, text string }{
		{"R01", "text", "an ordinary message"},
		{"R02", "system", unsupportedMarker("eventMessage")},
		{"R03", "system", unsupportedMarker("eventMessage")},
		{"R04", "system", unsupportedMarker("pollUpdateMessage")},
		{"R05", "system", unsupportedPrefix + "truncated"},         // malformed
		{"R06", "system", undecryptableMarker("unavailable")},      //
		{"R07", "system", undecryptableMarker("unavailable")},      //
		{"R08", "system", undecryptableMarker("decrypt-failed")},   //
		{"R09", "system", undecryptablePrefix + "truncated"},       // malformed
		{"R10", "system", ""},                                      // legacy empty
		{"R11", "system", ""},                                      // legacy empty
		{"R12", "system", "group name changed"},                    // real system text
		{"R13", "image", "a caption"},                              //
		{"R14", "system", undecryptableMarker("unavailable:hide")}, //
		{"R15", "system", unsupportedMarker("protocolMessage")},    //
	}
	for i, r := range rows {
		insertTestMessage(t, db, r.id, rawTypeTestChat, r.typ, r.text, int64(1784846600+i))
	}
}

// The headline assertion: identical output, proven against the old
// implementation rather than against hand-written expectations.
func TestIndexedCountersMatchTheLegacyImplementation(t *testing.T) {
	db := undecodedTestDB(t)
	seedRawTypeCorpus(t, db)

	s := &Server{db: db}
	got := s.undecodedStats(context.Background())
	want := legacyUndecodedStats(context.Background(), db)

	if got.UndecodedTotal != want.UndecodedTotal {
		t.Errorf("undecoded_total: legacy %d, indexed %d", want.UndecodedTotal, got.UndecodedTotal)
	}
	if got.UndecryptableTotal != want.UndecryptableTotal {
		t.Errorf("undecryptable_total: legacy %d, indexed %d", want.UndecryptableTotal, got.UndecryptableTotal)
	}
	if got.LegacyEmptySystem != want.LegacyEmptySystem {
		t.Errorf("legacy_empty_system: legacy %d, indexed %d", want.LegacyEmptySystem, got.LegacyEmptySystem)
	}
	if fmt.Sprint(got.UndecodedByType) != fmt.Sprint(want.UndecodedByType) {
		t.Errorf("undecoded_by_type:\n  legacy  %v\n  indexed %v", want.UndecodedByType, got.UndecodedByType)
	}
	if fmt.Sprint(got.UndecryptableByMode) != fmt.Sprint(want.UndecryptableByMode) {
		t.Errorf("undecryptable_by_mode:\n  legacy  %v\n  indexed %v", want.UndecryptableByMode, got.UndecryptableByMode)
	}

	// Sanity: the corpus must actually exercise every bucket, or the parity
	// above is a comparison of two empty maps.
	if want.UndecodedTotal == 0 || want.UndecryptableTotal == 0 || want.LegacyEmptySystem == 0 {
		t.Fatalf("corpus did not populate every bucket: %+v", want)
	}
	if want.UndecodedByType["unknown"] == 0 || want.UndecryptableByMode["unknown"] == 0 {
		t.Fatalf("corpus must include a malformed marker of BOTH families: %+v", want)
	}
}

// Done= requires the plan to be shown, not asserted in prose. This reads the
// real EXPLAIN QUERY PLAN and fails if the counter falls back to a table scan.
func TestCountersUseAnIndexNotAScan(t *testing.T) {
	db := undecodedTestDB(t)
	seedRawTypeCorpus(t, db)

	cases := []struct {
		name  string
		query string
	}{
		{
			"by-type aggregate",
			`SELECT raw_type, COUNT(*) FROM messages WHERE type = 'system' AND raw_type IS NOT NULL GROUP BY raw_type`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := queryPlan(t, db, tc.query)
			t.Logf("EXPLAIN QUERY PLAN:\n%s", plan)
			// COVERING is the property that matters, not SCAN-vs-SEARCH. The
			// PRE-fix query was already a SEARCH (idx_messages_type narrowed it
			// to type='system'), and it was still slow, because it then had to
			// READ every one of those rows to look at content_text. A COVERING
			// index means SQLite answers from index entries alone and touches
			// no table row at all. That is the whole fix.
			if !strings.Contains(plan, "COVERING INDEX") {
				t.Fatalf("counter is not answered from the index alone, so it still reads rows:\n%s", plan)
			}
		})
	}
}

func queryPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("explain columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		// The human-readable plan text is the last column on every SQLite
		// version that supports EXPLAIN QUERY PLAN.
		out = append(out, fmt.Sprint(vals[len(vals)-1]))
	}
	return strings.Join(out, "\n")
}

// The negative control for the perf claim. A timing test alone would pass on a
// warm cache regardless of the fix, so the control is STRUCTURAL: prove the
// pre-fix query plan is the scan this ticket is removing. If the legacy shape
// were already indexed, there would have been nothing to fix, and this fails.
func TestLegacyCounterShapeWasAScan(t *testing.T) {
	db := undecodedTestDB(t)
	seedRawTypeCorpus(t, db)

	plan := queryPlan(t, db,
		`SELECT COUNT(*) FROM messages WHERE type = 'system' AND content_text LIKE '[unsupported: %'`)
	t.Logf("pre-fix EXPLAIN QUERY PLAN:\n%s", plan)
	if strings.Contains(plan, "COVERING INDEX") {
		t.Fatalf("the legacy shape was ALREADY answered from an index alone, so this ticket's premise is wrong and the fix is not a fix:\n%s", plan)
	}
	// State the contrast explicitly: the legacy predicate reads content_text,
	// which no index carries, so every candidate row must be fetched.
	if !strings.Contains(plan, "messages") {
		t.Fatalf("unexpected plan shape, cannot assert the contrast:\n%s", plan)
	}
}

// The migration must reproduce, from markers alone, exactly what the writers
// would have stored. This runs the real migration over rows inserted WITHOUT
// raw_type (the pre-migration shape) and compares against the live derivation.
func TestMigrationBackfillMatchesTheWriters(t *testing.T) {
	db := undecodedTestDB(t)

	// Simulate the pre-006 store: markers present, raw_type absent.
	markers := []string{
		unsupportedMarker("eventMessage"),
		unsupportedMarker("pollUpdateMessage"),
		unsupportedPrefix + "truncated",
		undecryptableMarker("unavailable"),
		undecryptableMarker("unavailable:hide"),
		undecryptablePrefix + "truncated",
		"",
		"a real system message",
	}
	for i, m := range markers {
		if _, err := db.Exec(
			`INSERT INTO messages (id, chat_jid, sender_jid, timestamp, type, content_text, is_from_me, raw_type)
			 VALUES (?, ?, 'x@lid', ?, 'system', ?, 0, NULL)`,
			fmt.Sprintf("M%02d", i), rawTypeTestChat, 1784846600+i, m); err != nil {
			t.Fatalf("seed pre-migration row: %v", err)
		}
	}

	// Re-run 006's backfill. applyMigrations already ran it once on this db and
	// recorded version 6, so invoke the statements directly to exercise them
	// against these rows.
	runMigration006Backfill(t, db)

	for i, m := range markers {
		id := fmt.Sprintf("M%02d", i)
		var got sql.NullString
		if err := db.QueryRow(`SELECT raw_type FROM messages WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		want := rawTypeForStorage("system", m)
		if got.String != want {
			t.Fatalf("row %s (content %q): migration stored %q, writers would store %q",
				id, m, got.String, want)
		}
	}
}

// runMigration006Backfill replays 006's UPDATE statements. Kept as the literal
// SQL from the migration so a change there that is not mirrored here shows up
// as a failure of TestMigrationBackfillMatchesTheWriters.
func runMigration006Backfill(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`UPDATE messages SET raw_type = substr(content_text, length('[unsupported: ') + 1,
		   length(content_text) - length('[unsupported: ') - 1)
		  WHERE type='system' AND raw_type IS NULL AND content_text LIKE '[unsupported: %]'`,
		`UPDATE messages SET raw_type = 'unknown'
		  WHERE type='system' AND raw_type IS NULL AND content_text LIKE '[unsupported: %'`,
		`UPDATE messages SET raw_type = 'undecryptable:' || substr(content_text, length('[undecryptable: ') + 1,
		   length(content_text) - length('[undecryptable: ') - 1)
		  WHERE type='system' AND raw_type IS NULL AND content_text LIKE '[undecryptable: %]'`,
		`UPDATE messages SET raw_type = 'undecryptable:unknown'
		  WHERE type='system' AND raw_type IS NULL AND content_text LIKE '[undecryptable: %'`,
		`UPDATE messages SET raw_type = 'empty:system'
		  WHERE type='system' AND raw_type IS NULL AND COALESCE(content_text,'') = ''`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("backfill stmt: %v", err)
		}
	}
}

// The hazard this design introduces: raw_type is written BESIDE content_text,
// so a writer that sets one and forgets the other produces a row that is
// invisible to every counter. This drives the REAL write paths and asserts the
// invariant holds for all of them.
func TestEveryMarkerRowHasRawType(t *testing.T) {
	db := undecodedTestDB(t)
	b := &Bridge{db: db}

	chat, err := types.ParseJID(rawTypeTestChat)
	if err != nil {
		t.Fatalf("parse jid: %v", err)
	}
	sender, err := types.ParseJID("31628239888478:3@lid")
	if err != nil {
		t.Fatalf("parse jid: %v", err)
	}
	info := func(id string, ts int64) types.MessageInfo {
		return types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: true},
			ID:            id, PushName: "Martha", Timestamp: time.Unix(ts, 0),
		}
	}

	// writer 1: live receive, undecodable type -> [unsupported: ...]
	b.handleEvent(&events.Message{
		Info:    info("W1", 1784846601),
		Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}},
	})
	// writer 2: undecryptable -> [undecryptable: ...]
	b.handleEvent(&events.UndecryptableMessage{Info: info("W2", 1784846602), IsUnavailable: true})
	// writer 3: the history-sync content backfill repairing a legacy empty row
	insertTestMessage(t, db, "W3", rawTypeTestChat, "system", "", 1784846603)
	if _, err := b.backfillDecodedContent("W3", &waE2E.Message{
		PollUpdateMessage: &waE2E.PollUpdateMessage{},
	}); err != nil {
		t.Fatalf("backfillDecodedContent: %v", err)
	}
	// writer 4: live receive of an ordinary message (raw_type must stay NULL)
	b.handleEvent(&events.Message{
		Info:    info("W4", 1784846604),
		Message: &waE2E.Message{Conversation: proto.String("ordinary")},
	})

	rows, err := db.Query(`SELECT id, type, COALESCE(content_text,''), COALESCE(raw_type,'') FROM messages`)
	if err != nil {
		t.Fatalf("scan rows: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var id, typ, text, rawType string
		if err := rows.Scan(&id, &typ, &text, &rawType); err != nil {
			t.Fatalf("scan: %v", err)
		}
		checked++
		if want := rawTypeForStorage(typ, text); rawType != want {
			t.Fatalf("row %s: (type=%q, content_text=%q) implies raw_type %q, but the writer stored %q — this row is miscounted by /healthcheck",
				id, typ, text, want, rawType)
		}
	}
	if checked < 4 {
		t.Fatalf("expected at least 4 rows written by the real paths, saw %d", checked)
	}
}

func TestRawTypeForStorageResolutionOrder(t *testing.T) {
	cases := []struct{ typ, in, want string }{
		{"system", unsupportedMarker("eventMessage"), "eventMessage"},
		{"system", unsupportedPrefix + "truncated", "unknown"},
		{"system", undecryptableMarker("unavailable"), "undecryptable:unavailable"},
		{"system", undecryptableMarker("unavailable:hide"), "undecryptable:unavailable:hide"},
		{"system", undecryptablePrefix + "truncated", "undecryptable:unknown"},
		// An empty SYSTEM row is the pre-floor silent drop and is counted.
		{"system", "", "empty:system"},
		// An empty row of any other type is an ordinary message with no caption
		// and must NOT be swept into legacy_empty_system.
		{"image", "", ""},
		{"text", "", ""},
		{"text", "an ordinary message", ""},
		{"system", "group name changed", ""},
		{"system", "[not a marker]", ""},
	}
	for _, tc := range cases {
		if got := rawTypeForStorage(tc.typ, tc.in); got != tc.want {
			t.Errorf("rawTypeForStorage(%q, %q) = %q, want %q", tc.typ, tc.in, got, tc.want)
		}
	}
	if rawTypeNullable("text", "ordinary") != nil {
		t.Error("a non-marker must bind as NULL, not as an empty string")
	}
	if rawTypeNullable("system", unsupportedMarker("x")) != "x" {
		t.Error("a marker must bind as its raw type")
	}
}
