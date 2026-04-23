-- whatsapp-mcp: initial schema
-- Applied on bridge startup. Idempotent via IF NOT EXISTS.
-- Owns: contacts, chats, messages, calls, media_downloads, sends, audit_log.

-- Contacts. Phone + LID (Linked IDentifier) + JID are all possible keys.
CREATE TABLE IF NOT EXISTS contacts (
    jid TEXT PRIMARY KEY,
    lid TEXT,                          -- whatsmeow LID, opaque linked-identifier form
    phone TEXT,                        -- E.164 phone where available
    push_name TEXT,                    -- the name the contact set for themselves
    verified_name TEXT,                -- WhatsApp Business verified name if applicable
    normalized_name TEXT,              -- NFD-normalized, lowercase, stripped-diacritics form for accent-insensitive search
    is_business INTEGER NOT NULL DEFAULT 0,
    last_seen INTEGER,                 -- unix epoch of last known presence, nullable
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_contacts_normalized_name ON contacts(normalized_name);
CREATE INDEX IF NOT EXISTS idx_contacts_phone ON contacts(phone);
CREATE INDEX IF NOT EXISTS idx_contacts_lid ON contacts(lid);

-- Chats (direct, group, broadcast, community).
CREATE TABLE IF NOT EXISTS chats (
    jid TEXT PRIMARY KEY,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('direct', 'group', 'broadcast', 'community')),
    name TEXT,                         -- group name or contact push_name for direct chats
    normalized_name TEXT,
    participants_count INTEGER,        -- nullable for direct chats
    last_message_id TEXT,
    last_message_time INTEGER,
    last_message_preview TEXT,         -- short preview for list_chats output
    unread_count INTEGER NOT NULL DEFAULT 0,
    is_archived INTEGER NOT NULL DEFAULT 0,
    is_pinned INTEGER NOT NULL DEFAULT 0,
    is_muted INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chats_last_message_time ON chats(last_message_time DESC);
CREATE INDEX IF NOT EXISTS idx_chats_normalized_name ON chats(normalized_name);
CREATE INDEX IF NOT EXISTS idx_chats_unread ON chats(unread_count) WHERE unread_count > 0;

-- Messages. One row per message including voice notes, media, system events.
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,               -- whatsmeow message ID
    chat_jid TEXT NOT NULL,
    sender_jid TEXT,                   -- null for system messages
    sender_display TEXT,               -- "Name (phone)" form for output convenience
    timestamp INTEGER NOT NULL,        -- unix epoch
    type TEXT NOT NULL CHECK (type IN (
        'text', 'image', 'video', 'audio', 'voice', 'document',
        'sticker', 'location', 'contact', 'call', 'system', 'reaction'
    )),
    content_text TEXT,                 -- text body, OR caption for media
    content_normalized TEXT,           -- NFD-normalized content for accent-insensitive search
    media_path TEXT,                   -- local filesystem path after download, nullable until downloaded
    media_mime TEXT,
    media_duration_sec INTEGER,        -- for audio/video
    quoted_message_id TEXT,            -- for reply-quotes, points at the quoted message
    reactions_json TEXT,               -- JSON array of {emoji, sender_jid, timestamp}
    is_from_me INTEGER NOT NULL DEFAULT 0,
    is_edited INTEGER NOT NULL DEFAULT 0,
    is_deleted INTEGER NOT NULL DEFAULT 0,
    voice_note_transcript TEXT,        -- Whisper transcription, nullable until transcribed
    voice_note_transcript_backend TEXT,-- 'local-cpp' or 'openai-api' or null
    voice_note_transcript_at INTEGER,  -- unix epoch when transcribed
    scrubbed_text TEXT,                -- prompt-injection-scrubbed representation shown to Claude
    scrub_flags_json TEXT,             -- JSON array of scrub patterns that matched, for audit
    FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_messages_sender_time ON messages(sender_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_messages_type ON messages(type);
CREATE INDEX IF NOT EXISTS idx_messages_content_normalized ON messages(content_normalized);
CREATE INDEX IF NOT EXISTS idx_messages_quoted ON messages(quoted_message_id);

-- Calls: voice/video, 1:1 and group, inbound and outbound.
CREATE TABLE IF NOT EXISTS calls (
    id TEXT PRIMARY KEY,
    chat_jid TEXT,
    caller_jid TEXT,
    timestamp INTEGER NOT NULL,
    duration_sec INTEGER,              -- null for missed/rejected
    call_type TEXT NOT NULL CHECK (call_type IN ('voice', 'video')),
    is_group INTEGER NOT NULL DEFAULT 0,
    is_outbound INTEGER NOT NULL DEFAULT 0,
    result TEXT CHECK (result IN ('answered', 'missed', 'rejected', 'ended', 'failed')),
    FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
CREATE INDEX IF NOT EXISTS idx_calls_chat_time ON calls(chat_jid, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_calls_caller_time ON calls(caller_jid, timestamp DESC);

-- Media downloads: track which media files have been pulled to disk.
CREATE TABLE IF NOT EXISTS media_downloads (
    message_id TEXT PRIMARY KEY,
    original_filename TEXT,
    local_path TEXT NOT NULL,
    size_bytes INTEGER,
    checksum_sha256 TEXT,
    downloaded_at INTEGER NOT NULL,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

-- Sends: two-step draft / confirm pattern.
-- A send tool creates a row here with status='draft'.
-- confirm_send(draft_id) flips status to 'confirmed' and triggers the actual network send.
-- On success, status transitions to 'sent' and whatsapp_message_id is populated.
CREATE TABLE IF NOT EXISTS sends (
    draft_id TEXT PRIMARY KEY,
    recipient_jid TEXT NOT NULL,
    recipient_display TEXT,
    send_type TEXT NOT NULL CHECK (send_type IN (
        'text', 'file', 'audio', 'reply_quote', 'reaction'
    )),
    content_text TEXT,
    content_file_path TEXT,
    content_media_mime TEXT,
    quoted_message_id TEXT,
    reaction_emoji TEXT,
    status TEXT NOT NULL CHECK (status IN ('draft', 'confirmed', 'sent', 'failed', 'expired')),
    created_at INTEGER NOT NULL,
    confirmed_at INTEGER,
    sent_at INTEGER,
    whatsapp_message_id TEXT,
    error_message TEXT
);
CREATE INDEX IF NOT EXISTS idx_sends_status ON sends(status, created_at DESC);

-- Audit log: every tool call. Rotated daily; retained N days per env config.
CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp INTEGER NOT NULL,
    tool_name TEXT NOT NULL,
    params_json TEXT,                  -- params with media redacted to metadata only
    result_summary TEXT,
    duration_ms INTEGER,
    error_message TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_tool ON audit_log(tool_name, timestamp DESC);

-- Schema version tracking.
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL,
    description TEXT NOT NULL
);
INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (1, strftime('%s', 'now'), 'Initial schema: contacts, chats, messages, calls, media_downloads, sends, audit_log');
