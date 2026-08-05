# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **A transient secret-store read failure can no longer destroy the DB key (MYC-3694).** The bridge minted a fresh SQLCipher key on ANY failed keychain read and wrote it with overwrite semantics (`security add-generic-password -U`), so a locked keychain (exit 51) silently replaced the real key and permanently orphaned the encrypted message store. Reads are now classified on all three platforms — macOS exit 44 / Windows `ERROR_NOT_FOUND` / libsecret silent miss are the only "mint" states; every other failure exits loudly, writes nothing, and names the remedy (unlock the store, or set `WHATSAPP_DB_KEY`). The macOS write dropped `-U` (create-only), and minting is refused outright while a non-empty store database exists.

### Changed

- `scripts/check_prerequisites.sh` no longer hard-fails on `ffmpeg` or the `sqlcipher` CLI: transcription is off by default (ffmpeg is only needed for `local-cpp`), and the bridge compiles its own SQLCipher (the CLI was never a runtime requirement — the checker itself was a leftover stuck-point). Both downgraded to warnings with accurate guidance, matching `check_prerequisites.ps1`.
- The bridge logs one line at startup when transcription is off, so users upgrading from the old `local-cpp` default see the behavior change instead of losing transcripts silently.
- README manual-install block aligned with v0.2.0 reality: prebuilt binaries first, FFmpeg/whisper listed as optional, pairing-code alternative mentioned.

### Removed

- Tracked `whatsapp-mcp.mcpb` at the repo root — it was a stale v0.1.x build that nothing referenced; the canonical bundle is the release asset (`releases/latest/download/whatsapp-mcp.mcpb`), rebuilt per release.

## [0.2.0] - 2026-07-13

### Added

- **Windows support, end to end (MYC-3033).** Native Windows Credential Manager storage for the SQLCipher key (direct advapi32 syscalls, no new dependency) with a real round-trip test on the windows CI runner; the long-documented `WHATSAPP_DB_KEY` env override is now actually implemented (it previously existed only in error messages and docs); Windows-safe terminal QR rendering (ANSI block fallback outside Windows Terminal, drawn through go-colorable so legacy conhost translates it); `scripts/check_prerequisites.ps1`; SETUP.md rewritten with per-OS commands (PowerShell/winget/MSYS2 UCRT64, correct `.exe` paths, `%USERPROFILE%` layouts); `.mcpb` manifest `platform_overrides` launches via `python` on win32 instead of the usually-absent `python3`; the install-ping hook falls back `python3 → py -3 → python`.
- **Live-refreshing QR + pairing-code auth.** The terminal QR redraws in place on every rotation (attempt counter + expiry note) instead of stacking stale codes down the scrollback; when a whole batch expires the bridge requests fresh codes automatically instead of exiting; `--pair-phone +15551234567` pairs by typed 8-char code (identical on Android and iOS) for terminals that can't render a QR at all; non-TTY runs emit one parseable log line per rotation instead of ANSI art.
- **Headless auth API (feeds the Mycelium Studio connector, MYC-961).** The HTTP server now starts BEFORE pairing, so the API is live during first-run auth: `auth_state` machine + `logged_out_reason` + `qr_expires_at` on `GET /api/status`; `GET /api/auth/qr` (raw rotating code for a GUI to render); `POST /api/auth/pair-phone`; `POST /api/auth/reconnect`. WhatsApp-side logouts re-enter pairing automatically without a process restart. Read responses carry a `_bridge_state` envelope so cached SQLite data is distinguishable from a live session. `WHATSAPP_BRIDGE_PORT=0` picks a free port, announced via a `BRIDGE_LISTENING port=N` line + a `bridge.port` sidecar file for multi-instance supervisors.
- **`.env` file support.** SETUP has always said `cp .env.example .env`, but nothing ever read that file — installs silently ran on defaults. The bridge now loads `.env` from the working directory or next to the binary; explicit process env always wins.
- **Cross-platform CI + release binaries.** `tests.yml` now runs the Go + Python suites on ubuntu/macos/windows (MSYS2 UCRT64 gcc provides CGO on the Windows runner); new `release.yml` builds and attaches prebuilt bridge binaries (+ SHA256 files) for all three OSes to every published GitHub release — Windows users skip the C toolchain entirely.

- **Python test suite** under `whatsapp-mcp-server/tests/`. Mirrors the Go scrubber test corpus pattern: known-pattern coverage in lowercase / mixed-case / prose-sandwich variants, false-positive corpus, multi-pattern in one message, and a documented "known gaps" set (skipped, parity with Go). Also covers `lookup_crm_context` (tempdir-backed CRM fixtures) and `_normalize_phone`. 98 passing / 10 skip-known-gaps.
- **Go test suite for `normalize.go` and `aliases.go`.** `normalize_test.go` covers ASCII, Spanish accents, European diacritics, idempotence, and non-Latin passthrough. `aliases_test.go` covers the pure helpers (`inClausePlaceholders`, `jidsToArgs`) and the DB-backed `recordJIDAlias` + `resolveAliases` against in-memory SQLite, including the symmetric-edge invariant that was the original LID/phone JID split regression.
- **`.github/workflows/tests.yml`** runs the full Go + Python test suite on every PR + push (broader than `scrubber-eval.yml`, which only fires on scrubber-file changes).
- **`docs/TROUBLESHOOTING.md`** documents operational pain points: QR pairing failures, `StreamReplaced`, LID/phone alias split, voice-note transcription prereqs, SQLCipher Keychain backup, `uv sync` parent-dir quirk, bridge `/healthcheck` 502, audit-log rotation.
- **`SECURITY.md` webhook integrity contract.** Documents the three controls (HMAC signature, per-delivery idempotency key, retry queue) that the webhook feature MUST ship with — codified before implementation so the feature can't ship without them.

### Changed

- **`WHATSAPP_WHISPER_BACKEND` defaults to `off` (was `local-cpp`).** The old default made a ~3 GB whisper model a boot requirement — `Config.Validate()` refused to start without `WHATSAPP_WHISPER_MODEL_PATH`, stranding every fresh install on every OS. Transcription stays fail-loud once explicitly enabled, and media keys are persisted regardless, so enabling a backend later backfills recent voice notes. Set `WHATSAPP_WHISPER_BACKEND=local-cpp` explicitly if you relied on the old default.
- Vault-export filenames escape Windows-reserved device stems (`CON`, `NUL`, `COM1`…) and trailing dots/spaces.
- whisper/ffmpeg runtime error messages give per-OS install hints (previously Homebrew-only).
- Python MCP: bridge failures now surface the Go side's structured `{error, details}` body instead of an opaque HTTP status string, and stderr is forced to UTF-8 on Windows so Spanish text in log lines isn't silently dropped.

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
