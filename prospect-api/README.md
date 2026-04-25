# prospect-api

A small Go service that exposes a strict, public-facing surface for an AI Concierge to:

- Recognize an incoming guest (existence + relationship + category enum, never content)
- Pull deterministic context for a verified-identity guest
- Update CRM fields through a server-enforced whitelist
- Check arrival against a pre-warmed admin preset

It is a sibling of the `whatsapp-bridge` in this repository. It reads the
bridge's encrypted SQLite database (read-only) for messages and contacts,
and it owns its own preset + audit + ping-budget database.

## Why it exists

The `whatsapp-bridge` exposes a permissive HTTP surface intended for a single
trusted local Claude Code session. The `prospect-api`, by contrast, is the
surface that goes out to the public internet (via Cloudflare Tunnel) so an
AI Concierge running on Vercel — or any other authorized consumer — can
recognize guests without ever touching message content.

## Architecture

```
                  ┌──────────────────────────────┐
   Concierge ────►│  POST /api/lookup-prospect   │
   (Vercel)       │  POST /api/pull-context      │   prospect-api
                  │  POST /api/update-crm        │   (this binary)
                  │  POST /api/check-preset      │   port 8081
                  │  POST /api/preset (admin)    │   loopback only
                  └────────────┬─────────────────┘
                               │
                               ▼
                  ┌──────────────────────────────┐
                  │  whatsapp-bridge SQLite DB   │ read-only
                  │  ~/.claude/whatsapp-mcp/...  │
                  │  (SQLCipher-encrypted)       │
                  └──────────────────────────────┘
                               │
                               ▼
                  ┌──────────────────────────────┐
                  │  vault CRM .md files         │ read + whitelisted write
                  └──────────────────────────────┘
```

Public exposure is exclusively through Cloudflare Tunnel. The Go binary itself
binds 127.0.0.1.

## Security posture

See `SECURITY-PROSPECT.md`. Highlights:

- Loopback-only bind (validated at startup).
- Two trust levels: lookup is low-trust (existence + category, no content);
  pull-context is gated by server-side identity verification.
- Server-enforced field whitelist on all CRM writes. Default-safe: only blanks
  filled, never overwrites unless explicit.
- Rate limited per token (60/min) and per phone (5/min).
- Audit log on every request. No external network calls (Anthropic, OpenAI)
  in the lookup or update path.
- The `/api/preset` endpoint is on a separate admin token, higher trust.

## Status

- Phase A (this commit): the five endpoints above, deterministic categorizer,
  CRM read/write whitelist, preset + audit DB, two-tier rate limit, server-side
  identity verification on pull-context.
- Phase B (Sprints 7–10, subsequent commits): `/api/get-negotiator-terms`,
  `/api/notify-adelaida`, `/api/relay-note`, `/api/morning-digest`.

## Build + run

See `SETUP.md`.

## License

MIT. Same threat-model + non-affiliation disclaimer as the parent
`whatsapp-mcp` repository.
