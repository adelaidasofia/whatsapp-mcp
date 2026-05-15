-- 002: voice/audio media-key columns + index for transcript backfill.
-- Background: voice notes used to enqueue transcription via an in-memory
-- closure capturing the live whatsmeow message. If the worker failed
-- (validation, queue, transient error), the message lost its media
-- handle the moment the receive handler returned. This migration
-- persists the fields whatsmeow.Client.Download needs so we can
-- reconstitute an AudioMessage and re-pull from WhatsApp's CDN.

ALTER TABLE messages ADD COLUMN media_key BLOB;
ALTER TABLE messages ADD COLUMN media_direct_path TEXT;
ALTER TABLE messages ADD COLUMN media_url TEXT;
ALTER TABLE messages ADD COLUMN media_enc_sha256 BLOB;
ALTER TABLE messages ADD COLUMN media_sha256 BLOB;
ALTER TABLE messages ADD COLUMN media_file_length INTEGER;
ALTER TABLE messages ADD COLUMN media_key_timestamp INTEGER;

-- Sweeper-friendly partial index: only voice/audio rows with re-download
-- data and no transcript. Keeps the periodic scan O(pending) not O(all).
CREATE INDEX IF NOT EXISTS idx_messages_voice_pending
    ON messages(timestamp DESC)
    WHERE type IN ('voice', 'audio')
      AND voice_note_transcript IS NULL
      AND media_key IS NOT NULL;

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (2, strftime('%s', 'now'), 'Audio media-key columns + sweeper index for transcript backfill');
