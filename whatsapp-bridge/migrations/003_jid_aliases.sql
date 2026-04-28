-- 003: jid_aliases — symmetric JID alias map (LID ↔ phone-number JID).
--
-- Background: WhatsApp's privacy rollout migrated direct chats to
-- LID-form JIDs (e.g. 221298374504478@lid) while older threads stay on
-- @s.whatsapp.net. The whatsmeow event stream gives us BOTH forms via
-- MessageSource.Sender + MessageSource.SenderAlt (and Chat + RecipientAlt
-- for DMs), but until now the bridge stored each form as a separate
-- contact + chat row with no link. search_contacts then matched only one
-- row by name, list_messages returned only that row's history, and any
-- contact whose recent thread lived under @lid looked silent for months.
-- See: 2026-04-28 Eleonora Morales debug session.
--
-- Fix architecture:
--   1. Symmetric edges: insert (A,B) and (B,A) for every observation.
--      Lookups become a one-row PK hit either direction.
--   2. Source field tracks how the alias was learned for audit + debugging.
--   3. discovered_at lets us age out stale mappings if WhatsApp rotates LIDs
--      (not currently observed, but cheap insurance).
--
-- Idempotent: PRIMARY KEY (jid_a, jid_b) makes re-runs no-ops.

CREATE TABLE IF NOT EXISTS jid_aliases (
    jid_a TEXT NOT NULL,
    jid_b TEXT NOT NULL,
    discovered_at INTEGER NOT NULL,
    source TEXT NOT NULL,
    PRIMARY KEY (jid_a, jid_b)
);

CREATE INDEX IF NOT EXISTS idx_jid_aliases_b ON jid_aliases(jid_b);

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (3, strftime('%s', 'now'), 'jid_aliases: symmetric LID ↔ PN alias map');
