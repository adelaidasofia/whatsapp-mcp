# whatsapp-mcp


<!-- mycelium-badges:start -->

<p>
  <a href="https://github.com/adelaidasofia/whatsapp-mcp/blob/main/LICENSE"><img alt="License" src="https://img.shields.io/github/license/adelaidasofia/whatsapp-mcp?color=blue"></a>
  <a href="https://github.com/adelaidasofia/whatsapp-mcp/stargazers"><img alt="GitHub stars" src="https://img.shields.io/github/stars/adelaidasofia/whatsapp-mcp?color=eab308"></a>
  <a href="https://github.com/adelaidasofia/whatsapp-mcp/commits/main"><img alt="Last commit" src="https://img.shields.io/github/last-commit/adelaidasofia/whatsapp-mcp"></a>
  <a href="https://github.com/adelaidasofia/whatsapp-mcp/issues"><img alt="Open issues" src="https://img.shields.io/github/issues/adelaidasofia/whatsapp-mcp"></a>
  <a href="https://pypi.org/project/adelaidasofia-whatsapp-mcp/"><img alt="PyPI version" src="https://img.shields.io/pypi/v/adelaidasofia-whatsapp-mcp?color=blue&label=pypi"></a>
  <a href="https://pypi.org/project/adelaidasofia-whatsapp-mcp/"><img alt="PyPI downloads" src="https://img.shields.io/pypi/dm/adelaidasofia-whatsapp-mcp?color=blue&label=downloads"></a>
  <a href="https://myceliumai.co"><img alt="Built by Mycelium AI" src="https://img.shields.io/badge/built_by-Mycelium_AI-15B89A"></a>
</p>

<!-- mycelium-badges:end -->

<!-- mcp-name: io.github.adelaidasofia/whatsapp-mcp -->

A WhatsApp MCP server for Claude, built directly on [whatsmeow](https://github.com/tulir/whatsmeow). Encrypted at rest, prompt-injection-scrubbed, draft-and-confirm on every send, full audit trail, daily CI security gates. Actively maintained.

## Why this one?

The most-starred WhatsApp MCP (`lharries/whatsapp-mcp`, 5.6K stars) is the architectural reference for this pattern, but has not shipped since July 2025 and leaves the [lethal-trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) problem entirely to the user. This implementation closes the gaps:

| | Canonical | This implementation |
|---|---|---|
| Last shipped | July 2025 | Active |
| DB encryption | Plain SQLite | SQLCipher with key in the platform secret store (macOS Keychain / Windows Credential Manager / libsecret) |
| Prompt-injection scrubber | None | Every inbound message |
| Send safety | Fires immediately | Mandatory `confirm_send` between draft and delivery |
| Audit log | None | Every tool call, 30-day retention |
| Voice notes | Not transcribed | `whisper.cpp` local, Spanish-tuned default |
| LID alias resolution | Open issue cluster upstream | Shipped, with backfill migration for legacy threads |
| CI security | None | `govulncheck` + `pip-audit` + Dependabot, daily |

Not a fork. The Go bridge is built directly against `whatsmeow`; the Python MCP layer and SQLite schema are original. Other implementations (`lharries`, `LukasHaas`, `verygoodplugins`) were read as reference only.

## What this gives you

Claude can:

- Read your WhatsApp chats, messages, and contacts
- Search messages with accent-insensitive, typo-tolerant matching
- Transcribe voice notes locally via `whisper.cpp` (Spanish-tuned by default)
- Resolve LID (Linked IDentifier) names instead of numeric placeholders
- Send text messages, reactions, and reply-quotes, with a mandatory `confirm_send` step between draft and delivery
- Pull matching CRM context from your Obsidian vault when reading a chat
- See only prompt-injection-scrubbed message text, never raw adversarial input

Everything runs locally on your machine. No cloud sync. No telemetry. Optional OpenAI Whisper backend is opt-in, off by default.

## Architecture

Two components, both local:

- **`whatsapp-bridge/`** (Go). Binds to 127.0.0.1 only. Wraps `whatsmeow` for the WhatsApp Web multidevice protocol. Owns SQLite persistence with SQLCipher encryption. Handles QR and pairing-code auth (live-refreshing terminal QR with Windows-safe rendering, plus a headless auth API — `GET /api/auth/qr`, `POST /api/auth/pair-phone`, `POST /api/auth/reconnect` — so a GUI or supervisor can drive pairing without a terminal), media up/download, session recovery from `StreamReplaced` conflicts, call history capture. Exposes a REST API the Python MCP layer consumes.
- **`whatsapp-mcp-server/`** (Python, FastMCP). Consumes the Go bridge REST API. Exposes 11 MCP tools to Claude: full read surface (chats, messages, contacts), accent-insensitive search, presence (typing, online, mark-read), and text-send + reactions + reply-quotes with mandatory `confirm_send`. Runs via `uv` and stdio transport.

## Install

Open Claude Code, paste:

    /plugin marketplace add adelaidasofia/whatsapp-mcp
    /plugin install whatsapp-mcp@whatsapp-mcp

This installs the Python MCP server side. The Go bridge still needs the one-time QR pairing flow with your phone — see the legacy install block below for those steps.

<details>
<summary>Legacy install (manual, full Go bridge + QR pairing)</summary>

See [SETUP.md](SETUP.md) for step-by-step install. In short:

1. Prereqs: Go 1.24+, Python 3.11+, FFmpeg, uv
2. Clone this repo
3. Run `scripts/check_prerequisites.sh`
4. Start the bridge: `cd whatsapp-bridge && go run .`
5. Scan the QR code with WhatsApp on your phone (Settings, Linked Devices, Link a Device)
6. Register the MCP in your Claude Code `.mcp.json`
7. Restart Claude Code

</details>

## Configuration

All configurable via environment variables. See [.env.example](.env.example) for the full list.

Key variables:

| Variable | Default | Purpose |
|---|---|---|
| `WHATSAPP_BRIDGE_PORT` | `8080` | Go bridge REST API port |
| `WHATSAPP_DB_PATH` | `$HOME/.claude/whatsapp-mcp/store/messages.db` | Encrypted SQLite database |
| `WHATSAPP_MEDIA_PATH` | `$HOME/.claude/whatsapp-mcp/media/` | Media file storage |
| `WHATSAPP_VAULT_CRM_PATH` | empty | Absolute path to your vault CRM folder for auto-injection (e.g., Obsidian `👤 CRM/`). When unset, CRM injection is disabled. |
| `WHATSAPP_WHISPER_BACKEND` | `off` | `off` (zero-config boot), `local-cpp` (private, needs a whisper model), or `openai-api` (opt-in) |
| `WHATSAPP_WHISPER_API_KEY` | empty | Required only when backend is `openai-api` |
| `WHATSAPP_WHISPER_MODEL` | `large-v3` | whisper.cpp model name |
| `WHATSAPP_SCRUB_PROMPT_INJECTION` | `true` | Strip known prompt-injection patterns from incoming messages before Claude sees them |
| `WHATSAPP_AUDIT_LOG` | `true` | Log every tool call to `audit.log` |
| `WHATSAPP_ENCRYPT_DB` | `true` | Enable SQLCipher DB encryption; key in the platform secret store (macOS Keychain / Windows Credential Manager / libsecret) |
| `WHATSAPP_DB_KEY` | empty | Explicit 64-hex-char SQLCipher key override for headless/CI/custom secret managers (skips the platform store) |

## Security

This MCP is the highest-trust component in your Claude stack because every WhatsApp message you receive flows through it. See [SECURITY.md](SECURITY.md) for the threat model, tool risk-tier classification, and the full list of hardening decisions.

Short version:

- Bridge binds to `127.0.0.1` only, never `0.0.0.0`
- SQLite encrypted at rest with SQLCipher; key stored in the platform secret store (macOS Keychain / Windows Credential Manager / libsecret), with an explicit `WHATSAPP_DB_KEY` escape hatch
- Every tool call logged to `audit.log` with 30-day retention
- Send tools require an explicit `confirm_send` step between draft and delivery
- Incoming message text passes through a prompt-injection scrubber before Claude sees it
- `whatsmeow` pinned to a specific commit; upgrades require diff review
- No telemetry, no external API calls by default

## Status

v0.1.0, actively maintained.

Shipped: live-refreshing QR (in-place redraw on rotation, Windows-safe rendering, auto fresh batch on expiry) + pairing-code auth (typed 8-char code, Android + iOS) + headless auth API for supervisors, full read surface (chats, messages, contacts), accent-insensitive NFD-normalized search, LID alias resolution with backfill migration for legacy threads, Baileys-store import for one-shot history migration, vault-format markdown export, local `whisper.cpp` voice transcription, presence (typing, online, mark-read), text-send with mandatory `confirm_send`, reactions, reply-quotes, prompt-injection scrubber, SQLCipher-encrypted persistence with macOS Keychain key handling, audit log, CI security gates.

Not yet shipped: media-send (image, document), audio-message-send (FFmpeg-Opus path), group broadcast helpers.

See [CHANGELOG.md](CHANGELOG.md) for full history.

## MCP Registry

Published on the [official MCP Registry](https://registry.modelcontextprotocol.io/v0.1/servers?search=io.github.adelaidasofia/whatsapp-mcp) under `io.github.adelaidasofia/whatsapp-mcp`. Two live channels:

- **`.mcpb` bundle** (canonical, recommended) — one-click install in Claude Desktop / Cursor / any [MCPB](https://github.com/modelcontextprotocol/mcpb)-aware client. Published as a GitHub release artifact at [releases/latest/download/whatsapp-mcp.mcpb](../../releases/latest/download/whatsapp-mcp.mcpb). The release manifest carries the SHA256 for tamper detection.
- **PyPI package** (`adelaidasofia-whatsapp-mcp`) — historical; available via `uvx adelaidasofia-whatsapp-mcp` for stdio-installer flows. The unprefixed names (`whatsapp-mcp`, `whatsapp-mcp-server`) are taken by unrelated projects on PyPI, hence the username-prefixed namespace.

The verification marker `mcp-name: io.github.adelaidasofia/whatsapp-mcp` is embedded in this README (HTML comment near the top) so the registry can verify package-to-server ownership at publish time.

**Publishing pipeline:** built and shipped via the Mycelium MCP publishing pipeline (two-phase: `.mcpb` bundle build, then `gh release` + `mcp-publisher publish`). The same pipeline produced all 16 sibling MCPs in this family.

## Related MCPs

Same author, same architecture pattern (FastMCP, draft+confirm on writes where applicable, vault auto-export, MIT):

- [slack-mcp](https://github.com/adelaidasofia/slack-mcp) — multi-workspace Slack
- [imessage-mcp](https://github.com/adelaidasofia/imessage-mcp) — macOS iMessage
- [google-workspace-mcp](https://github.com/adelaidasofia/google-workspace-mcp) — Gmail / Calendar / Drive / Docs / Sheets
- [apollo-mcp](https://github.com/adelaidasofia/apollo-mcp) — Apollo.io CRM + sequences
- [substack-mcp](https://github.com/adelaidasofia/substack-mcp) — Substack writing + analytics
- [luma-mcp](https://github.com/adelaidasofia/luma-mcp) — lu.ma events
- [parse-mcp](https://github.com/adelaidasofia/parse-mcp) — markitdown / Docling / LlamaParse router
- [rescuetime-mcp](https://github.com/adelaidasofia/rescuetime-mcp) — RescueTime productivity data
- [graph-query-mcp](https://github.com/adelaidasofia/graph-query-mcp) — vault knowledge graph queries
- [graph-autotagger-mcp](https://github.com/adelaidasofia/graph-autotagger-mcp) — wikilink suggestions from the graph
- [investor-relations-mcp](https://github.com/adelaidasofia/investor-relations-mcp) — seed-raise pipeline tracker
- [vault-sync-mcp](https://github.com/adelaidasofia/vault-sync-mcp) — bidirectional vault sync


## Telemetry

This plugin sends a single anonymous install signal to `myceliumai.co` the first time it loads in a Claude Code session on a given machine.

**What is sent:**
- Plugin name (e.g. `slack-mcp`)
- Plugin version (e.g. `0.1.0`)

**What is NOT sent:**
- No user identifiers, names, emails, tokens, or API keys
- No file paths, message content, or anything from your work
- No IP address is stored after dedup processing

**Why:** Helps the maintainer know which plugins people actually install, so attention goes to the ones that get used.

**Opt out:** Set the environment variable `MYCELIUM_NO_PING=1` before launching Claude Code. The hook will skip the network call entirely. Already-pinged installs leave a sentinel at `~/.mycelium/onboarded-<plugin>` — delete it if you want to reset state.

## License

MIT. See [LICENSE](LICENSE).

## Not affiliated with WhatsApp or Meta

WhatsApp is a trademark of Meta Platforms, Inc. This project is an independent open-source tool that uses WhatsApp's public web-multidevice protocol. Use of this tool may violate WhatsApp's Terms of Service. Use at your own risk. The authors provide no warranty and accept no liability for account suspension, data loss, or other consequences.

---

Built by Mycelium AI. MIT license.
