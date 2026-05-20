# Troubleshooting

Known operational pain points and how to recover. Each entry: symptom → cause → fix.

## QR pairing fails / "Couldn't link device"

**Symptom.** The terminal QR appears, you scan it from WhatsApp on your phone, and the bridge either hangs or prints a `pair: timeout` error.

**Cause.** Three usual suspects:
1. Phone-side WhatsApp version is older than the multidevice rollout.
2. Phone is on cellular while the bridge is on Wi-Fi (rare but observed).
3. The bridge already has a session and is trying to pair again instead of resuming.

**Fix.**
- Update WhatsApp on your phone to the latest version.
- Put your phone on the same network as the bridge for the pairing step (you can switch back after).
- If you previously paired this device, delete the `store/` directory before re-running the bridge. `store/` contains the persistent multidevice session; pairing skips when one already exists.

## "StreamReplaced" disconnect / bridge logs out

**Symptom.** The bridge was running fine, then suddenly logs `StreamReplaced` and stops receiving messages.

**Cause.** WhatsApp's multidevice protocol allows only one active session per linked-device slot. If the same slot is opened from another machine (or the WhatsApp Web browser tab), the older one gets evicted with `StreamReplaced`.

**Fix.**
- Make sure you're not running the bridge in two places simultaneously.
- Close any WhatsApp Web browser tabs that were paired to the same device slot.
- Restart the bridge. It will re-attach to the same session if `store/` is intact — no re-pair needed.

## Contact's recent messages look "silent" / search returns no history

**Symptom.** A contact you've definitely been messaging shows no recent messages, or `search_contacts` returns the contact but `list_messages` for that JID is empty.

**Cause.** WhatsApp's privacy rollout migrated most direct chats to LID-form JIDs (`<opaque>@lid`) while legacy threads stayed on `<phone>@s.whatsapp.net`. The bridge stored both forms as separate rows. Recent traffic for the same human can land under either.

**Fix.** This is handled automatically as of migration `003_jid_aliases.sql`:
- `search_contacts` returns both JID rows for a matched human, each listing the other in an `aliases` field.
- `list_messages` resolves the requested JID through `jid_aliases` and queries history under all known aliases, returning a `merged_jids` field when more than one form contributed.

If history still looks split, run `/healthcheck` and inspect the `alias_coverage` block. A non-zero `suspicious_lid_phones` count means at least one LID contact still has a stale phone column; restart the bridge to trigger `BackfillJIDAliases` which heals it.

## Voice notes have no transcript

**Symptom.** A voice note arrives, `list_messages` shows the message but the `voice_note_transcript` field is empty.

**Cause.** One of:
1. `whisper.cpp` model missing (`models/ggml-large-v3.bin` not downloaded).
2. FFmpeg not in `PATH` (whisper needs it to decode WhatsApp's `.ogg` Opus encoding).
3. The bridge ran out of disk space and aborted the transcription.
4. `WHATSAPP_WHISPER_BACKEND=openai-api` set but `WHATSAPP_WHISPER_API_KEY` missing.

**Fix.**
- `bash scripts/check_prerequisites.sh` runs all the checks (Go, Python, FFmpeg, SQLCipher, uv, whisper.cpp). It will tell you exactly which one is missing.
- For the Whisper model: download manually from the [whisper.cpp model repo](https://huggingface.co/ggerganov/whisper.cpp/tree/main) into `models/`. Default expected file is `models/ggml-large-v3.bin`.
- For FFmpeg on macOS: `brew install ffmpeg`. On Linux: `sudo apt-get install ffmpeg`.

## SQLCipher Keychain key lost — DB unreadable

**Symptom.** Bridge fails to start with `ping db (wrong key?)` after a Keychain reset, OS reinstall, or user-folder migration.

**Cause.** The SQLite database is encrypted at rest with a key stored in the macOS Keychain (`service=whatsapp-mcp`, `account=default`). If the Keychain entry is lost, the database is unrecoverable.

**Fix.**
- The encrypted database is unrecoverable without the key. This is by design — the threat model assumes a stolen DB file should be unreadable without the live Keychain entry.
- Recovery: rename `store/messages.db` (or delete it), re-pair from scratch via QR, and let WhatsApp backfill history. The bridge captures `history_sync` events for ~6 months of recent traffic on a freshly-paired device.
- Prevention: back up the Keychain item via `Keychain Access.app` → search "whatsapp-mcp" → File → Export Items. Stash that file in a secure location (1Password, encrypted USB, encrypted backup).

## macOS Keychain permission prompt loops on every bridge start

**Symptom.** Every time the bridge starts, macOS pops up a "do you want to allow Claude to access the Keychain?" prompt.

**Cause.** Claude.app's TCC grant for the Keychain item is per-application-binary and per-Keychain-item. If the binary changes (Claude.app updated, MCP server moved), the grant resets.

**Fix.**
- In the Keychain Access prompt, click "Always Allow" rather than "Allow" — that persists the grant for that binary.
- If prompts persist on every launch, the Keychain Access.app entry for `whatsapp-mcp` may have an "Access Control" tab with the wrong binary listed; correct it to point at the current Claude.app or current Python binary.

## `uv sync` fails on the Python MCP server

**Symptom.** `cd whatsapp-mcp-server && uv sync` fails with:
> distutils.errors.DistutilsOptionError: Cannot access '../README.md' (or anything outside the package root)

**Cause.** `pyproject.toml` references `readme = "../README.md"` and `license = { file = "../LICENSE" }`. Setuptools 77+ refuses to read files outside the package root at build time. The published wheel is built by an external pipeline (`⚙️ Meta/scripts/publish-mcps.py`) that stages the files first, so PyPI works — but a contributor running `uv sync` locally hits this.

**Fix (for contributors).** Install dependencies directly instead of building the package:
```bash
cd whatsapp-mcp-server
python -m pip install \
    'fastmcp>=3.2.4' 'httpx>=0.27.0' \
    'python-frontmatter>=1.1.0' 'unidecode>=1.3.8' \
    'pytest>=8.0' 'pytest-asyncio>=0.23'
pytest tests/
```

A proper fix lives at the build-pipeline level (staging README + LICENSE into the package dir, or using `dynamic = ["readme"]` with a different build backend). Tracked but not yet shipped.

## Bridge `/healthcheck` returns 502 / connection refused

**Symptom.** The Python MCP server logs `bridge healthcheck failed: ConnectError` on startup and tools error at call time.

**Cause.** The Go bridge isn't running, or it's bound to a different host/port than the Python layer expects.

**Fix.**
- Check the bridge is running: `ps aux | grep whatsapp-bridge`. If not, start it (`cd whatsapp-bridge && go run .` or run the prebuilt binary).
- Check the bridge port matches what the Python layer expects. Defaults are `127.0.0.1:8080` on both sides. Override via `WHATSAPP_BRIDGE_PORT` if you need a different port (both sides must agree).
- Tail `bridge-stdout.log` for startup errors (DB key, migrations, network).

## Audit log getting large

**Symptom.** `audit.log` is hundreds of MB and growing.

**Cause.** Every tool call writes a JSON line to `audit.log` with no built-in rotation in the Python layer (the SECURITY.md threat model assumes a daily-rotate at the OS layer via `logrotate` or similar).

**Fix.**
- macOS: install `logrotate` via Homebrew and add a config under `/usr/local/etc/logrotate.d/` for the audit log.
- Or set up a launchd plist that gzips + rotates `audit.log` daily.
- Or just `rm audit.log` periodically — it's append-only, so removing it is safe between bridge starts (the bridge re-creates it on next write).

The 30-day retention promise in `SECURITY.md` is a target, not yet automated. Track via the file's last-modified date and size.
