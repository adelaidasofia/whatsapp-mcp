// raw_type.go — the decode outcome as an INDEXED value rather than a string
// parsed back out of user-facing content (MYC-3577).
//
// /healthcheck's decode counters used to be derived with
// `content_text LIKE '[unsupported: %'` over every `system` row. Correct, and
// unscalable: measured on the live store, five consecutive calls took 14.75s,
// 8.65s, 2.88s, 3.19s and 1.48s. That spread is the signature of a scan, not of
// a slow query. A cold monitoring poll always pays the top of the range, and
// the `system` population grows ~400/day.
//
// It was also the wrong coupling. An operator-facing metric should not depend
// on the exact bytes rendered into a member's chat file.
//
// messages.raw_type (migration 006) records the outcome once, at write time, in
// a column that can be indexed and grouped. Every writer derives it through the
// ONE function below, so the value that gets counted and the marker that gets
// displayed cannot drift apart. That is the same single-declaration discipline
// unsupportedPrefix / undecryptablePrefix already use.
package main

import "strings"

// rawTypeNamespaceUndecryptable namespaces MYC-3569 decrypt failures inside the
// shared raw_type column, so ONE indexed GROUP BY serves both counter families
// and the split is done in Go over the small grouped result rather than over
// every row in SQL.
const rawTypeNamespaceUndecryptable = "undecryptable:"

// rawTypeUnknown is what an unparseable marker records. The pre-MYC-3577
// counters reported a malformed marker as "unknown" instead of dropping it, and
// that behavior is preserved deliberately: a row we cannot classify is still a
// row we failed to read, and silently excluding it from the total would be the
// same class of bug these counters exist to expose.
const rawTypeUnknown = "unknown"

// rawTypeEmptySystem marks the PRE-floor silent drops: a `system` row with no
// content at all, which is what MYC-3284's bug produced and what the backfill
// is driving to zero.
//
// They get a sentinel instead of being left NULL so that ALL THREE counters
// come from the ONE covering aggregate. The alternative was an index over
// (type, content_text), and measured on SQLite that is the only shape that
// covers a `content_text = ”` predicate — a partial index on the same
// predicate is simply never chosen. Indexing content_text would copy every
// message BODY into an index on a 75MB store, which is a real cost to pay for
// one counter.
//
// The colon is deliberate: proto field names are identifiers and can never
// contain one, so this can never collide with a real raw type recovered from a
// marker.
const rawTypeEmptySystem = "empty:system"

// rawTypeForStorage maps a row's stored (type, content_text) to the value
// messages.raw_type should hold, or "" for a row that decoded fine and belongs
// in no counter.
//
// It takes msgType as well as the text because "an empty `system` row" is a
// counted population and an empty row of any other type is not. Deriving that
// from content alone would either miss the legacy drops or sweep in ordinary
// empty captions.
//
// Resolution order matches the migration's backfill exactly, which is what
// makes the migrated numbers equal the pre-migration numbers:
//
//	well-formed [unsupported: X]     -> X
//	malformed   [unsupported: ...    -> "unknown"
//	well-formed [undecryptable: X]   -> "undecryptable:X"
//	malformed   [undecryptable: ...  -> "undecryptable:unknown"
//	empty `system` row               -> "empty:system"
//	anything else                    -> ""
func rawTypeForStorage(msgType, contentText string) string {
	if mode := undecryptableFailMode(contentText); mode != "" {
		return rawTypeNamespaceUndecryptable + mode
	}
	if raw := unsupportedRawType(contentText); raw != "" {
		return raw
	}
	// Prefix present but the marker did not round-trip: it is truncated or
	// otherwise malformed. Still counted, never dropped.
	if strings.HasPrefix(contentText, undecryptablePrefix) {
		return rawTypeNamespaceUndecryptable + rawTypeUnknown
	}
	if strings.HasPrefix(contentText, unsupportedPrefix) {
		return rawTypeUnknown
	}
	if msgType == "system" && contentText == "" {
		return rawTypeEmptySystem
	}
	return ""
}

// rawTypeNullable renders rawTypeForStorage for binding into SQL, where "" must
// become NULL. NULL is what "this row decoded fine and is in no counter" means,
// and it keeps the (type, raw_type) index to the rows the counters care about.
func rawTypeNullable(msgType, contentText string) any {
	if rt := rawTypeForStorage(msgType, contentText); rt != "" {
		return rt
	}
	return nil
}

// splitRawTypeCount routes one grouped (raw_type, count) pair into the right
// counter bucket. The prefix test runs over the GROUPED result (dozens of rows)
// rather than over the message table (tens of thousands), which is the whole
// point of namespacing every counted population into one indexed aggregate.
func splitRawTypeCount(rawType string, n int, st *decodeStats) {
	switch {
	case rawType == rawTypeEmptySystem:
		st.LegacyEmptySystem += n
	case strings.HasPrefix(rawType, rawTypeNamespaceUndecryptable):
		mode := strings.TrimPrefix(rawType, rawTypeNamespaceUndecryptable)
		if mode == "" {
			mode = rawTypeUnknown
		}
		st.UndecryptableByMode[mode] += n
		st.UndecryptableTotal += n
	default:
		if rawType == "" {
			rawType = rawTypeUnknown
		}
		st.UndecodedByType[rawType] += n
		st.UndecodedTotal += n
	}
}
