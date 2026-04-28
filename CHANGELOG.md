# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **JID alias resolution for LID + phone-number forms.** WhatsApp's privacy
  rollout migrated most direct chats to LID-form JIDs (`<opaque>@lid`) while
  legacy threads stayed on `<phone>@s.whatsapp.net`. Recent traffic for the
  same human can arrive under either form depending on the contact's privacy
  settings, and whatsmeow surfaces both forms via
  `MessageSource.SenderAlt` / `RecipientAlt`. The bridge previously stored
  each form as a separate contact + chat row with no link, so
  `search_contacts` returned only the row whose stored push_name matched the
  query string and `list_messages` returned only that JID's history — a
  contact whose recent thread had moved to LID looked silent under their
  legacy JID. Fix:
  - New migration `003_jid_aliases.sql` adds a symmetric `jid_aliases` edge
    table.
  - `aliases.go` introduces `BackfillJIDAliases`, which walks contacts at
    startup, asks `client.Store.LIDs.GetPNForLID` / `GetLIDForPN` for the
    counterpart form, and writes the edge.
  - `onMessage` now records alias edges from `Info.SenderAlt` (incoming DMs)
    and `Info.RecipientAlt` (outgoing DMs) on every event.
  - `onMessage` no longer stores `Sender.User` as `phone` for LID-form JIDs
    (where `User` is the opaque LID number, not a phone). Falls back to
    `SenderAlt.User` when the alt is the phone form, otherwise leaves
    `phone` NULL until the startup backfill resolves it.
  - `BackfillJIDAliases` repairs existing rows whose `phone` column was
    populated with the LID number from the legacy ingestion path.
  - `handleSearchContacts` returns both JID rows for any matched human, each
    with the other(s) listed in a new `aliases` field.
  - `handleListMessages` resolves the requested JID through
    `jid_aliases` and queries `WHERE chat_jid IN (jid + aliases)`. Adds a
    `merged_jids` field to the response when more than one form contributed.
  - `/healthcheck` exposes a new `alias_coverage` block:
    `total_edges`, `contacts_with_alias`, `direct_chats_lid`,
    `direct_chats_phone`, and the regression guard
    `suspicious_lid_phones` — non-zero means at least one LID contact still
    has its `phone` column populated with the LID number itself.

### Added

- Initial project scaffold.
- Repository structure: `whatsapp-bridge/` (Go), `whatsapp-mcp-server/` (Python), `migrations/`, `scripts/`, `docs/`.
- MIT license.
- `SECURITY.md` with full threat model and tool risk-tier classification.
- `SETUP.md` with step-by-step install for macOS, with Linux and Windows notes.
- `.env.example` documenting all configurable environment variables.
- `scripts/check_prerequisites.sh` validator for Go, Python, FFmpeg, SQLCipher, uv, and whisper.cpp.
- SQL schema in `whatsapp-bridge/migrations/001_initial.sql`: tables for contacts, chats, messages, calls, media_downloads, sends, audit_log.

### Architecture decisions

- Go bridge built directly on `go.mau.fi/whatsmeow`, not forked from any existing MCP.
- Python MCP layer uses `FastMCP` 3.x, matches the pattern used by other MCPs in the author's stack.
- SQLite with SQLCipher for encrypted-at-rest storage; key from macOS Keychain.
- Bridge binds to `127.0.0.1` only, never `0.0.0.0`.
- Voice-note transcription defaults to local `whisper.cpp` with `large-v3` model; OpenAI API is opt-in only.
- Vault CRM auto-injection reads from a configurable path (env var), defaulting to empty.
- Send tools require a two-step `draft` then `confirm_send` pattern. No one-shot sends.
- Prompt-injection scrubber on all incoming message text.
- Public GitHub repository from day 1.

## [0.1.0] - 2026-04-23

Initial public scaffold. Not yet functional end-to-end; auth and core tools land in subsequent commits.
