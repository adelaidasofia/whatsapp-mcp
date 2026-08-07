-- 004: outbound media draft options.
--
-- Background: the `sends` table has allowed send_type 'file' and 'audio'
-- since 001, and already carries content_file_path + content_media_mime.
-- What it never had was somewhere to keep the *choices* a media draft makes,
-- and those choices are made at draft time but only acted on at confirm:
--
--   * as_document — an image sent as a document keeps its original bytes
--     instead of being recompressed by WhatsApp. Matters for receipts,
--     scans and design files, so it cannot be inferred from the MIME type;
--     the same image/jpeg is a photo in one case and a document in another.
--
--   * voice_note — audio rendered as a PTT bubble rather than a file
--     attachment. Also not inferable: a user may well want to send an .ogg
--     as a plain attachment.
--
--   * filename — what the recipient sees. When bytes arrive inline as
--     base64 there is no path to derive a name from, so the caller supplies
--     it and it has to survive until the DocumentMessage is built.
--
-- Without these columns the confirm step would have to guess, and guessing
-- wrong is silent: the message sends, just not as the user asked.
--
-- Idempotent: every column is added with a default, so existing draft rows
-- (all of them text/reply_quote/reaction) keep working untouched.

ALTER TABLE sends ADD COLUMN media_filename TEXT;
ALTER TABLE sends ADD COLUMN media_as_document INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sends ADD COLUMN media_voice_note INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (4, strftime('%s', 'now'), 'sends: outbound media draft options (as_document, voice_note, filename)');
