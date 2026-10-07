-- 008: contacts.phone for every phone-number JID that is missing it.
--
-- contacts_sync.go wrote address-book rows with the number only inside the JID
-- (573001234567@s.whatsapp.net) and phone NULL. search_contacts matched the
-- phone column and never the JID, so a contact saved in the phone's address
-- book was found by name and, unless some other path had stored the number on
-- one of their rows, never by number. CRM enrichment matches on phone too, and
-- every search response reported an empty phone for those contacts. The writer
-- now stores phone; this repairs the rows it already wrote.
--
-- Only the phone-number form qualifies. The user part of a @lid JID is an
-- opaque identifier, and storing it as a phone is the earlier bug that the
-- SuspiciousLIDPhones counter in aliases.go still watches for. The digit check
-- skips any user part that is not a bare number (a device suffix, a malformed
-- row).
--
-- updated_at is left alone on purpose: search orders its results by it, and a
-- column filled in by a migration is not activity.
--
-- The rows it fills become visible to anything that matches on phone. CRM
-- enrichment (crm_enrich.go) is one: when a CRM folder is configured it runs
-- after every start and fills a blank push_name (with its normalized_name,
-- bumping updated_at) for contacts it matches by phone, which these rows now
-- are. That is the rule it already applied to a
-- @lid address-book row whose phone the alias backfill had filled. The vault
-- export shows push_name ahead of the chat name, so such a contact can then
-- be exported under its CRM name rather than its address-book name.

UPDATE contacts
   SET phone = SUBSTR(jid, 1, INSTR(jid, '@') - 1)
 WHERE (phone IS NULL OR phone = '')
   AND jid LIKE '%@s.whatsapp.net'
   AND INSTR(jid, '@') > 1
   AND SUBSTR(jid, 1, INSTR(jid, '@') - 1) NOT GLOB '*[^0-9]*';

INSERT OR IGNORE INTO schema_version (version, applied_at, description)
VALUES (8, strftime('%s', 'now'), 'contacts.phone filled from phone-number JIDs');
