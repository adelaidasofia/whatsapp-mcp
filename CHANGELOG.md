# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.1] - 2026-10-07

### Fixed

- **`search_contacts` could not find a saved contact by phone number.** Contacts that come from the phone's address book (`contacts_sync.go`) were stored with the number only inside the JID (`573001234567@s.whatsapp.net`) and an empty `phone` column, and the search matched `phone`, never the JID. Such a contact was found by name and returned nothing by number, unless some other path had also stored the number on one of their rows. A number typed the way a phone displays it (`+57 300 123 4567`) matched nothing even for contacts whose `phone` was set, because the column holds bare digits and the query was compared as typed. Reported from a Windows install on v0.4.1, with the cause traced to `handleSearchContacts`. The address-book writer now stores `phone` for phone-number JIDs; migration 008 fills it in on the rows already written; and the search matches a number on its digits against both `phone` and the number inside a phone-number JID, so it no longer depends on which writer filled the column. Name matches rank ahead of number matches, so a short number-shaped query such as `7-` cannot push a name match like "7-Eleven" past the limit. The user part of a `@lid` JID is an opaque identifier: the address-book writer and the Baileys importer no longer store it as a phone, and the JID match only considers phone-number JIDs. The importer also no longer invents a name for a nameless contact: a phone-number JID keeps the `+<phone>` placeholder, and any other JID gets no label rather than `+<LID digits>` or the raw JID. With a CRM folder configured, CRM enrichment, which also matches on phone, now reaches address-book contacts under its existing rules, as it already did for `@lid` address-book contacts whose phone the alias backfill had filled: a contact with no push name of its own gets the CRM name, so its vault export can switch from the address-book name to the CRM name. The test for this endpoint used to copy part of its `WHERE` clause; it now calls the handler.
- **Importing a Baileys store renamed chats and contacts with numbers that are not theirs.** `--import-baileys` named a chat that had no name in the store `+<digits>` whatever kind of JID it was, so a `@lid` chat showed its opaque LID and a group its id as if they were phone numbers, and a JID with no digits got the raw JID as its name. Imported over rows the bridge had already written, or imported a second time, those labels replaced real names: a group called "Familia Pérez" came back as `+120363000000000001`, a chat the address book named "Mi Amor" as `+573001234567`, and a nameless contact's `+<phone>` placeholder overwrote the contact's own `push_name`. The chat fallback now uses the same phone-number rule as the contact loop: only a phone-number chat gets the placeholder, and a chat the importer creates without a name is stored with no name (NULL, so a later history sync can still fill it). On both upserts, no name keeps the stored one, the placeholder only fills a contact or chat that has no name, and `normalized_name` takes the same branch as the name. For contacts this keeps a stored name findable by `search_contacts`, which matches names through `normalized_name` and the address-book `normalized_full_name`. Labels an earlier import already wrote, such as `+<LID digits>` on a `@lid` chat, are not cleared by re-importing; they stay until a real name replaces them.

## [0.5.0] - 2026-10-06

### Added

- **`WHATSAPP_WHISPER_ONLY_CHATS`: transcribe only the chats you name.** `WHATSAPP_WHISPER_EXCLUDE_CHATS` answers "never transcribe these", but a member who wants transcription on a single chat — typically their own "message yourself" chat on a personal number — had to list every other contact, and every contact added later. Worse, the exclude list can only be written after you know which chats exist, while the first backfill sweep runs about 30 seconds after the bridge starts: on a real install, enabling `local-cpp` with no exclude list transcribed 14 voice notes from other people's chats in that first sweep. The new variable uses the same accent-insensitive substring matching against the chat JID and name, inverted: when set, a chat matching no pattern is excluded. It lives in `chatExcluded`, so both the live enqueue path and the backfill sweep honor it. Exclude still wins over only, a chat lookup error still fails closed, and an empty chat JID is excluded when an allow-list is set. Prefer full JIDs: a bare phone number also matches groups created from that number, whose JID starts with it. Verified on the same install after rebuilding: the next sweep enqueued 0 notes. Both filters are now documented in `.env.example`.
- **`/api/status` says how long the socket has been down, and a watchdog retries a socket that stays down.** On 2026-08-24 the bridge sat at `connected=false, authenticated=true` for a full day with nothing reporting it. `disconnected_seconds` starts at the first disconnect of an outage and clears on connect, so a day-long outage no longer reads like a two-second blip. Past a 2-minute grace, a watchdog retries on a widening schedule (about ten attempts in the first half hour, then two an hour) and logs each attempt with its outcome, so the cause (405, TLS timeout, DNS) lands in the log the same day. It never restarts the process (#75).
- **`WHATSAPP_BACKUP_PATH` now takes backups.** It had been in `.env.example` since the first release and nothing read it. Snapshots use SQLite `VACUUM INTO`, so they are consistent on a live database, need no downtime and keep the SQLCipher encryption (a canary test checks that). Each one is written to a temp file and renamed only on success. A snapshot is taken at startup when the newest one is older than an interval, and on a timer after that. New settings: `WHATSAPP_BACKUP_INTERVAL_HOURS` (default 24, `0` disables) and `WHATSAPP_BACKUP_KEEP` (default 7, clamped to at least 1). A failed backup is logged and never stops the bridge, and startup logs whether backups are on (#79).
- **MCP tools for groups, media download and history backfill.** The bridge already served `GET /api/groups`, `POST /api/media/download` and `POST /api/admin/request-history`; the MCP server now exposes each of them as a tool (#83).

### Fixed

- **On Windows, the bridge crashed the moment it drew a QR on a console with VT processing on, outside Windows Terminal.** With `WT_SESSION` empty the renderer called `qrterminal.Generate`, which probes the terminal for sixel graphics before drawing. When the console already has VT processing on (the reporter's PowerShell session did; whether it is on depends on the terminal and shell that start the bridge), go-colorable hands back `os.Stdout` itself, so the probe gets past its writer check and calls `term.MakeRaw` on the output handle. That fails, and the probe's already-deferred `term.Restore` then dereferences the nil state it got back: `golang.org/x/term` `term_windows.go:47`. The QR is drawn on the login loop's goroutine, so the panic took the whole process down on every attempt to pair. Reported from a Windows 11 install on v0.4.1, three crashes in three attempts. The renderer now builds its `qrterminal.Config` itself (byte-identical output, pinned by a test against the old helpers, and no probe), a test fails if any bridge code references `qrterminal.Generate` or `qrterminal.IsSixelSupported` again, and a panic while drawing is logged (with its stack the first time) while pairing carries on through `GET /api/auth/qr` or `--pair-phone`. A Windows-only test rebuilds that console on the CI runner (real `CONOUT$`, VT on, `WT_SESSION` empty), keeps the upstream nil-pointer panic as its control, and reads the console buffer back to confirm the QR frame reached it. Workaround on v0.4.1: `$env:WT_SESSION = "1"` before starting the bridge.
- **`--pair-phone` and `POST /api/auth/pair-phone` failed with `info query returned status 400: bad-request`.** The bridge requested the typed code under the name `Chrome (whatsapp-mcp)`. whatsmeow's `PairPhone` documents that the name must be `Browser (OS)` with a common browser and OS, and that WhatsApp answers anything else with 400; Baileys hit the same 400 with `Chrome (Baileys)` and fixed it by changing the OS part (WhiskeySockets/Baileys@c17198e). The name was the same on every OS, so by that contract the typed-code path failed everywhere, not only on Windows where it was reported. It now sends `Chrome (Linux)` on every OS, the name mautrix-whatsapp sends in production: the OS part is only a label here, and one proven value beats a per-OS label WhatsApp might not accept. A test asserts the value where it is handed to whatsmeow.
- **The hint under a Windows full-block QR offered a command that did not work.** It suggested `curl -X POST http://127.0.0.1:8080/...`, which Windows PowerShell 5.1 rejects (`curl` is `Invoke-WebRequest` there) and which ignored `WHATSAPP_BRIDGE_PORT`. It now points at `--pair-phone`, which works from any shell.
- **v0.4.1 shipped with its version stamps still at 0.4.0.** `version.go`, `manifest.json`, `.claude-plugin/plugin.json` and `whatsapp-mcp-server/pyproject.toml` all said 0.4.0 at the v0.4.1 tag, and the release build does not override `bridgeVersion`, so the v0.4.1 bridge reports 0.4.0 at `/healthcheck`. All four now say 0.5.0, and `TestVersionStampsAgree` keeps them in line with `bridgeVersion` and requires a dated CHANGELOG section for it. `release.yml` now builds the tag's own commit on every trigger: a `workflow_dispatch` re-run used to build whatever branch it was started from, so it could replace a release's binaries with code outside the tag. It also refuses a tag that is not `v<bridgeVersion>`. The install signal in `hooks/install-ping.py` sent a hardcoded `1.0.0` for every release; it now reads the version from `plugin.json`.
- **The `calls` table could never accept a row, on any install, since 001.** `onCallOffer` inserts `result='offered'`, but the column was declared `CHECK (result IN ('answered','missed','rejected','ended','failed'))` — 'offered' is not in that set, so every insert failed the CHECK. The handler logs the error and returns, so nothing surfaced: the process stayed healthy, `/healthcheck` stayed green, `capture_calls` kept reporting `true`, and the feature recorded nothing. Measured on a live bridge (v0.4.1, 2026-08-26): 13 consecutive `onCallOffer: insert failed: CHECK constraint failed: calls` in one startup, with `SELECT COUNT(*) FROM calls` = 0. A second defect sat underneath it: `onCallTerminate` wrote `strings.ToLower(evt.Reason)` into the same CHECKed column, and `Reason` is not an enum — whatsmeow lifts it verbatim off the wire (`call.go:92`, `cag.String("reason")`), so WhatsApp chooses the string. `timeout` and `decline` are both real values and neither was storable; that UPDATE never raised only because, with no row ever inserted, it matched zero rows and returned success. Migration 007 widens the vocabulary with `offered` and `unknown`, adds `result_raw` holding the verbatim wire reason (unconstrained, because it exists to hold what the CHECK cannot), and the terminate path now normalizes through one shared helper — the same bounded-value / preserved-source pairing `messages.raw_type` uses in 006. The CHECK is widened rather than dropped: the constraint was not the mistake, writing an unnormalized wire value into a constrained column was. A terminate with no matching call row is now logged instead of vanishing.
- **`/api/status` no longer reports `connected` on a handshake that was never confirmed**, and it now carries the last client error with the time it happened (#84).
- **A photo or PDF sent as part of a multi-file (album) message could not be downloaded.** WhatsApp wraps each album item in `associatedChildMessage`, which the bridge's hand-written envelope list missed, so the item was stored as `[unsupported: associatedChildMessage]` with no media key. The bridge now unwraps every `FutureProofMessage` envelope generically, so album items and the other wrapper types decode to their content, and media from history sync keeps its download key through every envelope (#92).

## [0.4.1] - 2026-08-25

### Fixed

- **WhatsApp stopped accepting the bridge entirely: `Client outdated (405)`.** The `whatsmeow` pin had not moved since 2026-04-21, and on 2026-08-24 WhatsApp began refusing that client build outright. The failure is quiet in the worst way: the process stays up, the HTTP API keeps answering from the local database, the device stays paired, and `/api/status` reports `authenticated: true` — only `connected` flips to `false`. whatsmeow classifies 405 as `ConnectFailureClientOutdated` and excludes it from `isRetryableConnectError`, so nothing retries and nothing recovers on its own. Every read kept working and returning real historical data while nothing new arrived. Measured on a running bridge: last ingest 2026-08-24 16:37, one `Client outdated (405) connect failure (client version: 2.3000.1037753511)` line, then no reconnect. Pin moved to `v0.0.0-20260821141805-33cfac511629` (2026-08-21); `connected: true` verified against the live socket after rebuild, with the existing session reused — **no re-pairing and no QR scan required** (#74).

### Security

- The bump closes an **arbitrary-host fetch** reachable from inbound message content. At the previous pin, `Download()` passed `urlable.GetURL()` — an attacker-supplied protobuf field — to `downloadAndDecrypt` for any host except `web.whatsapp.net`. Upstream commit `9ff5508` removes it. The same range adds constant-time `hmac.Equal` at six MAC sites, a Noise certificate validity window, and a fix for a reachable CBC length-check panic. Full 104-commit review recorded on #74 per SECURITY.md item 6.
- **Disclosure, changed data profile:** upstream flips `SupportCallLogHistory` to `true` (`store/clientpayload.go:137`), so WhatsApp now sends call log history into the local store. Two new secrets are stored at rest in the session DB (`whatsmeow_nct_salt.salt`, an HMAC-SHA256 key, and `whatsmeow_device.companion_meta_nonce`). The dependency also gains a `static.whatsapp.net` code path, which this bridge never calls.

### Changed

- **Minimum Go for a source build is now 1.26**, up from 1.24. Not our choice: `go.mau.fi/whatsmeow` and `go.mau.fi/util` both declare `go 1.26.0`, so the module directive moved to match. `README.md` and `SETUP.md` updated. Installs using a prebuilt release binary need no compiler and are unaffected.
- Session-store migrations `14-nct-salt.sql` and `15-companion-meta-nonce.sql` run on first start. Both are additive (`CREATE TABLE` / `ADD COLUMN`) and declare `compatible with v8+`, so an older binary still reads the store. There is no down-migration mechanism; a real rollback is a file backup.

## [0.4.0] - 2026-08-19

### Fixed

- **A control character in a chat name failed the ENTIRE vault export on Windows.** `sanitizeFilename` replaced the Win32 forbidden punctuation set, appended `_` to reserved device stems, trimmed trailing dots/spaces and NFC-folded for APFS — but never stripped C0 controls. A WhatsApp group SUBJECT and a contact PUSH NAME can both contain a newline, and both are set by other people. On macOS/Linux `Team
Standup.md` is a legal filename, so the export wrote it and the run was green; Windows rejects the path outright, the unit lands in `errs[]`, and reconciliation counts a dropped chat — which fails the whole export by design. One awkward group subject took every other chat down with it. Measured on the same commit: FAIL on Windows, `ok` on Linux.
- **A sender-chosen message ID wrote files outside the media directory.** `media_download.go` built its output path as `filepath.Join(MediaPath, messageID+ext)` with no validation, and that ID is not ours — whatsmeow lifts it verbatim off the wire and the bridge stores it unmodified, so the SENDING client chooses it. `filepath.Join` cleans a path; it does not confine one, so `..\..\evil` escaped (and on Windows `\` separates too). `safeMediaStem` now reduces the ID to an allowlisted stem, appending a hash of the original whenever anything changed so two hostile IDs cannot collide onto one file. Benign IDs pass through byte-identical, so media already on disk still resolves.
- **A keychain authorization prompt could hang the bridge at startup, forever (macOS/Linux).** No secret-store subprocess had a timeout, and `GetOrCreateDBKey` runs before the HTTP server binds — so a `security` or `secret-tool` call that decided it needed user authorization waited for a human indefinitely, with nothing in the log to say why. Measured on the macOS CI runner: `security add-generic-password` blocked and was killed after 9m56s. Every such call now runs under a deadline (`WHATSAPP_KEYCHAIN_TIMEOUT_SECONDS`, default 120s) — generous enough for a human to answer a real dialog, bounded so headless installs get a named error pointing at `WHATSAPP_DB_KEY`. Windows is unaffected (CredReadW/CredWriteW syscalls, not a subprocess).
- **`scripts/check_prerequisites.sh` told people they needed Go when they did not.** It treated Go as required and exited on the FIRST miss, so anyone installing the documented way — prebuilt binary, no compiler — met the project with a red `FAIL Go not found`, and never saw the python/uv checks that actually matter. Now matches `check_prerequisites.ps1`: required = python 3.11+ and uv; go and a C compiler are source-build only; reports everything, never stops early.
- **Shell scripts were CRLF-mangled on every Windows clone, and failed open.** With no `.gitattributes`, Git for Windows' default `core.autocrlf=true` rewrote the tracked scripts, so `set -euo pipefail` never applied and `check_prerequisites.sh` exited 0 having verified nothing. `.gitattributes` now pins `eol=lf` for text and `eol=crlf` for `.ps1`.
- Loopback is not a trust boundary against the browser (#34): a cross-origin `Content-Type: text/plain` POST is a CORS simple request, so it is never preflighted and every state-changing route ran on the way in. Verified against the running bridge before the fix. An origin guard now refuses non-allowlisted `Origin` headers and pins `Host` to loopback literals against DNS rebinding. Non-browser clients (the Python MCP server, curl, supervisors) send no `Origin` and are unaffected; `WHATSAPP_ALLOWED_ORIGINS` allows a browser-based pairing GUI through.
- The release smoke test could not fail — `--help 2>&1 | head -5 || true` discarded the exit status twice, so a binary that crashed on startup still shipped. It now captures the output and asserts on it.
- `manifest.json` launched `python main.py` with no dependency resolution, failing on any machine without fastmcp/httpx/python-frontmatter/unidecode already installed. Now uses `uv run`, matching `.mcp.json`.
- `.mcp.json` set `WHATSAPP_WHISPER_BACKEND=local-cpp` on the MCP server process, which never reads that variable (it belongs to the Go bridge) — inert, contradicting the documented `off` default, and naming a backend unavailable on Windows. Removed.
- `.gitignore` covered `whatsapp-bridge/whatsapp-bridge` but not the Windows `.exe`, so `go build ./...` left a 45 MB untracked binary.

### Added

- **Prebuilt binaries for Intel Macs (`darwin-amd64`).** The release matrix built only `darwin-arm64`, so the "no compiler needed" install path silently did not exist for Intel Macs — those users were routed into a Go + Xcode source build without being told.
- Real-Keychain round-trip test on the macOS CI leg (`keychain_darwin_test.go`), mirroring the existing Windows one. The macOS and Linux keychain tests use a PATH shim, which proves the exit-code classification but never touches a real store — so the only key store proven end to end was the one fewest users run. This test is what surfaced the startup hang above.
- `gofmt` CI gate. Nothing checked formatting, which is how 21 of 91 tracked `.go` files drifted; all swept.
- `WHATSAPP_ALLOWED_ORIGINS` and `WHATSAPP_KEYCHAIN_TIMEOUT_SECONDS` (both documented in `.env.example`).

### Changed

- Version is now sourced from one place (`whatsapp-bridge/version.go`, overridable via `-ldflags`). `/healthcheck` reported `0.3.0` while the newest tag was `v0.3.1`, `plugin.json` said `1.0.0`, `manifest.json` `0.1.0` and `pyproject.toml` `0.1.1` — five numbers, no two alike. All aligned on 0.4.0.

- **A transient secret-store read failure can no longer destroy the DB key (MYC-3694).** The bridge minted a fresh SQLCipher key on ANY failed keychain read and wrote it with overwrite semantics (`security add-generic-password -U`), so a locked keychain (exit 51) silently replaced the real key and permanently orphaned the encrypted message store. Reads are now classified on all three platforms — macOS exit 44 / Windows `ERROR_NOT_FOUND` / libsecret silent miss are the only "mint" states; every other failure exits loudly, writes nothing, and names the remedy (unlock the store, or set `WHATSAPP_DB_KEY`). The macOS write dropped `-U` (create-only), and minting is refused outright while a non-empty store database exists.
- **Vault export dropped entire direct chats and filed group chats under person names, while exiting 0 (MYC-3555).** Three interacting defects: output filenames were keyed on display name alone, so a group whose stored name was a person's push name collided with that person's direct chat on one path; the unchanged-skip trusted the `last_message_ts` of whatever file sat at the path without checking whose `jid` it carried, so an active group occupying a person's filename suppressed that person's export forever (counted as "skipped-unchanged", rc=0); and the exporter never consulted `jid_aliases`, so the LID and phone forms of the same human exported as two colliding files each holding half the history. Now: one human = one file with both JID forms merged (`alias_jids` frontmatter lists the other forms), non-direct chats always carry their type in the filename (`Name (group).md`, JID digits appended on residual collisions), the unchanged-skip only fires when the existing file provably belongs to the chat, files left under pre-fix names are healed by rename, `--export-min-messages` counts across a person's merged forms, and a run that drops any enumerated chat exits non-zero NAMING the dropped chats plus logs a machine-checkable `export: reconcile ...` counts line. rc=0 now means complete.
- Group chats born from a live message are no longer named after whichever member sent the first message the bridge saw; the real subject and participant count now come from group metadata (startup sync + JoinedGroup event).
- Exported group files no longer fabricate a `phone:` from the group JID (and LID digits are never rendered as a phone); `participants_count` is now populated from stored group metadata, with a distinct-sender floor as fallback instead of the old constant 0 that slid under every group-size policy.

- `--reconcile-vault <dir>`: offline audit of an exported vault against the bridge DB. Prints greppable `MISSING` / `MISFILED` / `DRIFT` / `DUPLICATE` / `ORPHAN` findings (chat with messages but no file; group sitting in a person-named file; file whose `message_count` disagrees with the DB; one JID claimed by two files; file whose JID left the DB) and exits non-zero when any exist. Honors the same `--export-include-groups` / `--export-min-messages` filters as the export.
- Startup group-metadata sync (`SyncGroupMetadata`) writing every joined group's real subject + participant count into `chats`, healing rows the old live path had person-named.

- `scripts/check_prerequisites.sh` no longer hard-fails on `ffmpeg` or the `sqlcipher` CLI: transcription is off by default (ffmpeg is only needed for `local-cpp`), and the bridge compiles its own SQLCipher (the CLI was never a runtime requirement — the checker itself was a leftover stuck-point). Both downgraded to warnings with accurate guidance, matching `check_prerequisites.ps1`.
- The bridge logs one line at startup when transcription is off, so users upgrading from the old `local-cpp` default see the behavior change instead of losing transcripts silently.
- README manual-install block aligned with v0.2.0 reality: prebuilt binaries first, FFmpeg/whisper listed as optional, pairing-code alternative mentioned.

### Removed

- Tracked `whatsapp-mcp.mcpb` at the repo root — it was a stale v0.1.x build that nothing referenced; the canonical bundle is the release asset (`releases/latest/download/whatsapp-mcp.mcpb`), rebuilt per release.

### Known issues

- **macOS: a Keychain prompt on the second and later boots (#70).** The stored key item is created with `-T ""` (an empty trusted-application list), which per `security(1)` removes the default trust the creating app would get — so `/usr/bin/security` may not read the key back without the user approving it. First boot mints the key and works; later boots raise a Keychain dialog. Clicking **Always Allow** once resolves it permanently. Headless installs should set `WHATSAPP_DB_KEY`. Whether to keep that posture is a security tradeoff tracked in #70; the timeout above makes either answer survivable.

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
