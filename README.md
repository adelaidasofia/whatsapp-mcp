# whatsapp-mcp

A personal WhatsApp MCP server for Claude, built directly on [whatsmeow](https://github.com/tulir/whatsmeow). Full audit surface, encrypted-at-rest storage, Whisper voice-note transcription, accent-insensitive Spanish search, vault-native CRM integration, and a send-confirmation dry-run pattern that prevents replying to the wrong contact.

Not a fork of any existing WhatsApp MCP. The Go bridge is built directly against whatsmeow; the Python MCP layer and SQLite schema are original. Other implementations (lharries, LukasHaas, verygoodplugins) were read as reference only.

## What this gives you

Claude can:

- Read your WhatsApp chats, messages, and contacts
- Search messages with accent-insensitive, typo-tolerant matching
- Transcribe voice notes locally via `whisper.cpp` (Spanish-tuned by default)
- Resolve LID (Linked IDentifier) names instead of numeric placeholders
- Send text, media, voice, reactions, and reply-quotes (with mandatory confirmation)
- Pull matching CRM context from your Obsidian vault when reading a chat
- See only prompt-injection-scrubbed message text, never raw adversarial input

Everything runs locally on your machine. No cloud sync. No telemetry. Optional OpenAI Whisper backend is opt-in, off by default.

## Architecture

Two components, both local:

- **`whatsapp-bridge/`** (Go). Binds to 127.0.0.1 only. Wraps `whatsmeow` for the WhatsApp Web multidevice protocol. Owns SQLite persistence with SQLCipher encryption. Handles QR and pairing-code auth, media up/download, session recovery from `StreamReplaced` conflicts, call history capture. Exposes a REST API the Python MCP layer consumes.
- **`whatsapp-mcp-server/`** (Python, FastMCP). Consumes the Go bridge REST API. Exposes 20+ MCP tools to Claude. Runs via `uv` and stdio transport.

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
| `WHATSAPP_WHISPER_BACKEND` | `local-cpp` | `local-cpp` (private) or `openai-api` (opt-in) |
| `WHATSAPP_WHISPER_API_KEY` | empty | Required only when backend is `openai-api` |
| `WHATSAPP_WHISPER_MODEL` | `large-v3` | whisper.cpp model name |
| `WHATSAPP_SCRUB_PROMPT_INJECTION` | `true` | Strip known prompt-injection patterns from incoming messages before Claude sees them |
| `WHATSAPP_AUDIT_LOG` | `true` | Log every tool call to `audit.log` |
| `WHATSAPP_ENCRYPT_DB` | `true` | Enable SQLCipher DB encryption with key from macOS Keychain |

## Security

This MCP is the highest-trust component in your Claude stack because every WhatsApp message you receive flows through it. See [SECURITY.md](SECURITY.md) for the threat model, tool risk-tier classification, and the full list of hardening decisions.

Short version:

- Bridge binds to `127.0.0.1` only, never `0.0.0.0`
- SQLite encrypted at rest with SQLCipher; key stored in macOS Keychain
- Every tool call logged to `audit.log` with 30-day retention
- Send tools require an explicit `confirm_send` step between draft and delivery
- Incoming message text passes through a prompt-injection scrubber before Claude sees it
- `whatsmeow` pinned to a specific commit; upgrades require diff review
- No telemetry, no external API calls by default

## Status

Early. v0.1.0 is the initial scaffold with auth and core read tools. See [CHANGELOG.md](CHANGELOG.md) for current state.

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

## License

MIT. See [LICENSE](LICENSE).

## Not affiliated with WhatsApp or Meta

WhatsApp is a trademark of Meta Platforms, Inc. This project is an independent open-source tool that uses WhatsApp's public web-multidevice protocol. Use of this tool may violate WhatsApp's Terms of Service. Use at your own risk. The authors provide no warranty and accept no liability for account suspension, data loss, or other consequences.

---

Built by Adelaida Diaz-Roa. Full install or team version at [diazroa.com](https://diazroa.com).
