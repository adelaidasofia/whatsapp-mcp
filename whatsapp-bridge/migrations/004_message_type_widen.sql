-- 004: widen the messages.type CHECK constraint and record the decode path.
--
-- Background (MYC-3284): extractContent ended in `default: return "", "system"`,
-- so every message shape the decoder did not know became a type:'system' row
-- with empty content_text — indistinguishable from a message that genuinely
-- had no text. 46,949 of 113,449 rows in the live store were in that state.
--
-- Two schema changes are needed to fix it:
--
--   * raw_type: the decode path for every row ("ephemeralMessage>conversation",
--     "pollCreationMessageV3", "unknownFooMessage"). Makes the store
--     self-describing, drives /healthcheck's undecoded_by_type, and is what the
--     vault export prints in an undecodable-message placeholder.
--
--   * a wider type CHECK: 'poll', 'event' and 'unsupported' are new storage
--     types. SQLite cannot ALTER a CHECK constraint, so the table is rebuilt.
--
-- The rebuild is the risky half, so it is written to be boring:
--
--   - It is WRAPPED IN A TRANSACTION. applyMigrations runs each file as a
--     single db.Exec with no surrounding transaction, so every statement would
--     otherwise autocommit independently: a failure between DROP TABLE messages
--     and the RENAME would destroy the entire message history with no rollback.
--     On a 113k-row table that is the difference between a failed migration and
--     an unrecoverable one.
--
--   - No PRAGMA toggles. This database opens with foreign_keys=0 and
--     legacy_alter_table=0 (verified against the live store; its DSN sets only
--     the SQLCipher key, unlike the whatsmeow session DSN which sets
--     _foreign_keys=on). Those are exactly the settings the SQLite
--     table-rebuild procedure wants, and PRAGMA statements are no-ops inside a
--     transaction anyway — writing them here would be decoration that reads
--     like a safeguard.
--
--   - Every column is listed explicitly in the INSERT..SELECT. `SELECT *`
--     would silently misalign if 001/002 column order ever changed.
--
--   - Indexes are recreated after the copy, matching 001 and 002 exactly.
--     Verified against the live store: 6 indexes, 1 FK child
--     (media_downloads.message_id), no triggers, no views.

BEGIN;

CREATE TABLE messages_new (
    id TEXT PRIMARY KEY,
    chat_jid TEXT NOT NULL,
    sender_jid TEXT,
    sender_display TEXT,
    timestamp INTEGER NOT NULL,
    type TEXT NOT NULL CHECK (type IN (
        'text', 'image', 'video', 'audio', 'voice', 'document',
        'sticker', 'location', 'contact', 'call', 'system', 'reaction',
        -- added in 004
        'poll', 'event', 'unsupported'
    )),
    content_text TEXT,
    content_normalized TEXT,
    media_path TEXT,
    media_mime TEXT,
    media_duration_sec INTEGER,
    quoted_message_id TEXT,
    reactions_json TEXT,
    is_from_me INTEGER NOT NULL DEFAULT 0,
    is_edited INTEGER NOT NULL DEFAULT 0,
    is_deleted INTEGER NOT NULL DEFAULT 0,
    voice_note_transcript TEXT,
    voice_note_transcript_backend TEXT,
    voice_note_transcript_at INTEGER,
    scrubbed_text TEXT,
    scrub_flags_json TEXT,
    media_key BLOB,
    media_direct_path TEXT,
    media_url TEXT,
    media_enc_sha256 BLOB,
    media_sha256 BLOB,
    media_file_length INTEGER,
    media_key_timestamp INTEGER,
    -- new in 004
    raw_type TEXT,
    FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);

INSERT INTO messages_new (
    id, chat_jid, sender_jid, sender_display, timestamp, type,
    content_text, content_normalized, media_path, media_mime,
    media_duration_sec, quoted_message_id, reactions_json,
    is_from_me, is_edited, is_deleted,
    voice_note_transcript, voice_note_transcript_backend, voice_note_transcript_at,
    scrubbed_text, scrub_flags_json,
    media_key, media_direct_path, media_url, media_enc_sha256, media_sha256,
    media_file_length, media_key_timestamp, raw_type
)
SELECT
    id, chat_jid, sender_jid, sender_display, timestamp, type,
    content_text, content_normalized, media_path, media_mime,
    media_duration_sec, quoted_message_id, reactions_json,
    is_from_me, is_edited, is_deleted,
    voice_note_transcript, voice_note_transcript_backend, voice_note_transcript_at,
    scrubbed_text, scrub_flags_json,
    media_key, media_direct_path, media_url, media_enc_sha256, media_sha256,
    media_file_length, media_key_timestamp, NULL
FROM messages;

DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;

-- Indexes from 001.
CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_messages_sender_time ON messages(sender_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(type);
CREATE INDEX IF NOT EXISTS idx_messages_content_normalized ON messages(content_normalized);
CREATE INDEX IF NOT EXISTS idx_messages_quoted ON messages(quoted_message_id);

-- Index from 002 (voice transcript sweeper).
CREATE INDEX IF NOT EXISTS idx_messages_voice_pending
    ON messages(timestamp DESC)
    WHERE type IN ('voice', 'audio')
      AND voice_note_transcript IS NULL
      AND media_key IS NOT NULL;

-- New in 004: drives the backfill scan (empty legacy 'system' rows) and
-- /healthcheck's undecoded_by_type grouping. Partial, so it stays O(affected).
CREATE INDEX IF NOT EXISTS idx_messages_undecoded
    ON messages(type, raw_type)
    WHERE type IN ('system', 'unsupported');

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (4, strftime('%s', 'now'), 'Widen messages.type CHECK (poll/event/unsupported) + raw_type decode-path column');

COMMIT;
