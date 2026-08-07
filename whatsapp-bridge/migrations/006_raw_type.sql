-- 006: record the decode outcome in an INDEXED column (MYC-3577).
--
-- Background. /healthcheck's decode counters were derived by matching a marker
-- string inside user-facing content:
--
--   WHERE type = 'system' AND content_text LIKE '[unsupported: %'
--
-- PR #43 scoped that to the indexed `type` subset, which bought an order of
-- magnitude, but the residual is still a prefix scan over every `system` row,
-- and that population grows ~400/day. Measured on the live store 2026-08-04,
-- five consecutive calls: 14.75s, 8.65s, 2.88s, 3.19s, 1.48s. The spread is the
-- tell: cold cache pays for reading ~50k rows off disk, warm cache does not. A
-- monitoring poll after an idle period always pays the cold price.
--
-- Deriving a COUNTER by parsing a string out of CONTENT is also the wrong
-- coupling. It means an operator-facing metric depends on the exact bytes shown
-- to a member in their chat file.
--
-- This migration is deliberately ADDITIVE. Stream A (branch
-- claude/myc-3284-decode-unsupported-message-types, 09c70fb) solved the same
-- problem with a full table REBUILD, because it also widened the messages.type
-- CHECK to add 'poll'/'event'/'unsupported'. We are NOT widening that CHECK
-- (MYC-3284 settled on the marker-in-content_text design precisely to avoid a
-- rebuild), so the column can just be added and nothing has to be copied.
--
-- The transaction lesson from A carries over even so, and is why this file
-- opens with BEGIN: applyMigrations runs each migration as a SINGLE db.Exec
-- with NO surrounding transaction, so without an explicit one every statement
-- below would autocommit independently. A failure partway through the backfill
-- would leave the column half-populated and the counters silently wrong, with
-- no rollback and nothing recording that it happened.
--
-- Value scheme for raw_type (namespaced so ONE indexed GROUP BY serves both
-- counter families, with the split done in Go over the small grouped result
-- rather than over 50k rows in SQL):
--
--   'pollUpdateMessage'         an MYC-3284 undecodable type, raw proto name
--   'unknown'                   an MYC-3284 marker too malformed to parse
--   'undecryptable:<mode>'      an MYC-3569 decrypt failure, mode as recorded
--   'undecryptable:unknown'     an MYC-3569 marker too malformed to parse
--   NULL                        everything else, including legacy empty rows
--
-- Writers derive this through ONE shared helper (rawTypeForStorage), the same
-- single-declaration discipline the markers themselves use, so what is written
-- and what is counted cannot drift.

BEGIN;

ALTER TABLE messages ADD COLUMN raw_type TEXT;

-- Backfill, in the same order the Go reader resolves these, so the migrated
-- numbers equal the pre-migration numbers exactly:
--
--   1. well-formed [unsupported: X]     -> X
--   2. any remaining [unsupported: ...  -> 'unknown'   (malformed, still counted)
--   3. well-formed [undecryptable: X]   -> 'undecryptable:X'
--   4. any remaining [undecryptable:... -> 'undecryptable:unknown'
--
-- Steps 2 and 4 are what keep the totals identical. The existing counters match
-- on the OPENING prefix only and report an unparseable marker as "unknown"
-- rather than dropping it; a backfill that required the closing bracket would
-- quietly shrink undecoded_total.

UPDATE messages
   SET raw_type = substr(content_text,
                         length('[unsupported: ') + 1,
                         length(content_text) - length('[unsupported: ') - 1)
 WHERE type = 'system'
   AND raw_type IS NULL
   AND content_text LIKE '[unsupported: %]';

UPDATE messages
   SET raw_type = 'unknown'
 WHERE type = 'system'
   AND raw_type IS NULL
   AND content_text LIKE '[unsupported: %';

UPDATE messages
   SET raw_type = 'undecryptable:' || substr(content_text,
                         length('[undecryptable: ') + 1,
                         length(content_text) - length('[undecryptable: ') - 1)
 WHERE type = 'system'
   AND raw_type IS NULL
   AND content_text LIKE '[undecryptable: %]';

UPDATE messages
   SET raw_type = 'undecryptable:unknown'
 WHERE type = 'system'
   AND raw_type IS NULL
   AND content_text LIKE '[undecryptable: %';

-- 5. The PRE-floor silent drops: an empty `system` row, which is what MYC-3284's
--    bug produced and what legacy_empty_system counts. They get a sentinel
--    rather than staying NULL so that ALL THREE counters come from the ONE
--    covering aggregate below.
--
--    The alternative was an index over (type, content_text). Measured on SQLite,
--    that is the only shape that covers a `content_text = ''` predicate: a
--    PARTIAL index on exactly that predicate is never chosen by the planner,
--    verified directly rather than assumed. Indexing content_text would copy
--    every message BODY into an index on a 75MB store, which is a real cost for
--    one counter.
--
--    The colon is deliberate. Proto field names are identifiers and can never
--    contain one, so this cannot collide with a raw type recovered from a marker.
UPDATE messages
   SET raw_type = 'empty:system'
 WHERE type = 'system'
   AND raw_type IS NULL
   AND COALESCE(content_text, '') = '';

-- The counter index. (type, raw_type) is a COVERING index for
-- "GROUP BY raw_type WHERE type='system' AND raw_type IS NOT NULL": SQLite
-- answers it from the index alone and never touches a table row. Confirmed with
-- EXPLAIN QUERY PLAN in raw_type_test.go rather than assumed here.
CREATE INDEX IF NOT EXISTS idx_messages_type_rawtype ON messages(type, raw_type);

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (6, strftime('%s', 'now'), 'messages.raw_type + indexed decode counters (MYC-3577)');

COMMIT;
