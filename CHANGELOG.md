# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Python test suite** under `whatsapp-mcp-server/tests/`. Mirrors the Go scrubber test corpus pattern: known-pattern coverage in lowercase / mixed-case / prose-sandwich variants, false-positive corpus, multi-pattern in one message, and a documented "known gaps" set (skipped, parity with Go). Also covers `lookup_crm_context` (tempdir-backed CRM fixtures) and `_normalize_phone`. 98 passing / 10 skip-known-gaps.
- **Go test suite for `normalize.go` and `aliases.go`.** `normalize_test.go` covers ASCII, Spanish accents, European diacritics, idempotence, and non-Latin passthrough. `aliases_test.go` covers the pure helpers (`inClausePlaceholders`, `jidsToArgs`) and the DB-backed `recordJIDAlias` + `resolveAliases` against in-memory SQLite, including the symmetric-edge invariant that was the original LID/phone JID split regression.
- **`.github/workflows/tests.yml`** runs the full Go + Python test suite on every PR + push (broader than `scrubber-eval.yml`, which only fires on scrubber-file changes).
- **`docs/TROUBLESHOOTING.md`** documents operational pain points: QR pairing failures, `StreamReplaced`, LID/phone alias split, voice-note transcription prereqs, SQLCipher Keychain backup, `uv sync` parent-dir quirk, bridge `/healthcheck` 502, audit-log rotation.
- **`SECURITY.md` webhook integrity contract.** Documents the three controls (HMAC signature, per-delivery idempotency key, retry queue) that the webhook feature MUST ship with — codified before implementation so the feature can't ship without them.

### Security

- **Sync Python scrubber pattern list with Go.** The Go scrubber gained 5 patterns (`ignore above instructions`, `disregard prior instructions`, `dump your system prompt`, `tell me your instructions`, `what are your instructions`) in the 2026-05-19 hardening pass; the Python layer kept the older 13-pattern list. Both scrubbers run in production (Go on incoming-from-protocol before DB write; Python before Claude sees text); drift between them was a defense-in-depth defect. New `test_pattern_count_matches_go` parity test fails CI on future drift.

### Fixed

- **pip-audit CI gate** had been failing on every push to main for at least 5 commits before this PR. Root cause: `whatsapp-mcp-server/pyproject.toml` referenced `readme = "../README.md"` and `license = { file = "../LICENSE" }`, and setuptools 77+ rejects any file ref outside the package root. The publish pipeline staged the files before build, so the published wheel was fine, but local builds + CI's `pip-audit` could never resolve the package. Fix: inline `readme = { text = "...", content-type = "text/markdown" }` and `license = "MIT"` (PEP 639 SPDX). PyPI page becomes minimal; the full README still lives at the repo root and is linked from the `[project.urls]` Homepage. The TROUBLESHOOTING.md `uv sync` entry now describes a fixed historical issue rather than a current one.

## [0.1.1] - 2026-05-19

### Added

- **Published to [MCP Registry](https://registry.modelcontextprotocol.io)** as `io.github.adelaidasofia/whatsapp-mcp` v0.1.1. PyPI path. Discovery-surface live for all MCP-aware clients.
- **PyPI package `adelaidasofia-whatsapp-mcp` v0.1.1** published via OIDC trusted-publisher. Description shortened to fit the registry's 100-char limit (v0.1.0 failed validation on the long-form description).
- **Scrubber CI gate** (`.github/workflows/scrubber-eval.yml`) running a 73-case eval corpus on every PR + push + weekly cron. Backs the security claim in SECURITY.md item 5 with actual enforcement.
- **whatsmeow upgrade-review workflow** (`.github/workflows/whatsmeow-upgrade-review.yml`) that intercepts any PR moving the whatsmeow pin, posts the upstream commit log as a PR comment, and applies a `whatsmeow-diff-review-required` label that must be manually cleared before merge.

## [0.1.0] - 2026-05-19

### Added

- **`.mcpb` bundle** built by the Mycelium MCP publishing pipeline and released as a GitHub artifact at [releases/v0.1.0](../../releases/tag/v0.1.0). One-click install in Claude Desktop / Cursor via the MCPB format.
- **Initial PyPI release** of `adelaidasofia-whatsapp-mcp` v0.1.0 (superseded by 0.1.1 for the registry's description-length constraint).
- **`server.json` registry manifest** with full 9-env-var schema covering bridge host/port, vault CRM path, Whisper backend selection, scrubber toggle, audit log toggle, and SQLCipher encryption toggle.

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
