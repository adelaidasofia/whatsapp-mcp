-- 007: make the calls table able to store a call.
--
-- Background. The `calls` table has never held a row, on any install, since it
-- was created in 001. `onCallOffer` inserts result='offered'; the column was
-- declared
--
--   result TEXT CHECK (result IN ('answered','missed','rejected','ended','failed'))
--
-- and 'offered' is not in that set, so every insert failed the CHECK. The
-- handler logs the error and returns, so nothing surfaced: the process stayed
-- healthy, /healthcheck stayed green, `capture_calls` still reported true, and
-- the feature recorded nothing. Measured on a live bridge (v0.4.1, 2026-08-26):
-- 13 consecutive `onCallOffer: insert failed: CHECK constraint failed: calls`
-- in a single startup, with `SELECT COUNT(*) FROM calls` = 0.
--
-- The second defect was hidden underneath the first. `onCallTerminate` wrote
-- strings.ToLower(evt.Reason) into the same CHECKed column, and Reason is not
-- an enum — whatsmeow lifts it verbatim off the wire (call.go:92,
-- `Reason: cag.String("reason")`), so WhatsApp picks the string. 'timeout' and
-- 'decline' are both real values and neither was storable. That UPDATE never
-- raised, because with no row ever inserted `WHERE id = ?` matched nothing and
-- returned success against zero rows. Fixing the INSERT alone would have turned
-- a silent no-op into a second CHECK failure, so both are fixed together.
--
-- Why a rebuild. SQLite cannot alter a CHECK in place; changing one means a new
-- table plus a copy. The copy below is written to preserve rows even though the
-- table is provably empty everywhere: "provably empty" rests on this code being
-- the only writer, and a migration is the wrong place to bet on that.
--
-- Design. `result` keeps a closed CHECK rather than dropping it. The constraint
-- was not the mistake — writing an unnormalized wire value into a constrained
-- column was. The vocabulary gains 'offered' (the state an offer is genuinely
-- in) and 'unknown' (a reason this version does not recognize), and the raw
-- wire string is kept alongside in `result_raw`. That pairing is the same shape
-- as messages.raw_type from 006: a bounded value for querying, the unbounded
-- source preserved for auditing. Without 'unknown', the next reason WhatsApp
-- invents re-breaks the write; with it, an unmapped reason costs a row that
-- says so and carries the evidence needed to extend the mapping.
--
-- The BEGIN is mandatory, for the reason recorded in 006: applyMigrations runs
-- each migration as a SINGLE db.Exec with no surrounding transaction, so
-- without one, a failure partway through would leave the table dropped and the
-- data gone with no rollback.

BEGIN;

CREATE TABLE calls_new (
    id TEXT PRIMARY KEY,
    chat_jid TEXT,
    caller_jid TEXT,
    timestamp INTEGER NOT NULL,
    duration_sec INTEGER,              -- null for missed/rejected
    call_type TEXT NOT NULL CHECK (call_type IN ('voice', 'video')),
    is_group INTEGER NOT NULL DEFAULT 0,
    is_outbound INTEGER NOT NULL DEFAULT 0,
    result TEXT CHECK (result IN ('offered', 'answered', 'missed', 'rejected', 'ended', 'failed', 'unknown')),
    -- The verbatim wire reason, NULL when the terminate carried none. Never
    -- constrained: this column exists precisely to hold what the CHECK cannot.
    result_raw TEXT,
    FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);

-- Column-explicit on both sides: a future ALTER on either table must not
-- silently re-map positions.
INSERT INTO calls_new (id, chat_jid, caller_jid, timestamp, duration_sec, call_type, is_group, is_outbound, result)
SELECT id, chat_jid, caller_jid, timestamp, duration_sec, call_type, is_group, is_outbound, result
  FROM calls;

DROP TABLE calls;

ALTER TABLE calls_new RENAME TO calls;

-- DROP TABLE took the indexes with it; 001 created both.
CREATE INDEX IF NOT EXISTS idx_calls_chat_time ON calls(chat_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_calls_caller_time ON calls(caller_jid, timestamp DESC);

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (7, strftime('%s', 'now'), 'calls.result vocabulary + result_raw: the table could never accept a row');

COMMIT;
