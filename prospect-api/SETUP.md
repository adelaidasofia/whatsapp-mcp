# prospect-api Setup

This service is the public-facing surface that an AI Concierge (or any
other consenting consumer) calls to recognize prospects, retrieve verified
context, and pre-warm guests.

It runs on the same Mac as the `whatsapp-bridge` (this binary has read-only
access to that bridge's encrypted SQLite database). Public exposure is via
Cloudflare Tunnel + Access policy (covered in `SETUP-CLOUDFLARE.md`).

## Prerequisites

- Go 1.24+
- The `whatsapp-bridge` has been started at least once on this machine
  (creates the SQLCipher key in macOS Keychain)
- The vault CRM folder exists locally and is reachable

## Configure

Copy the env template to a local file (never committed):

```bash
cd ~/.claude/whatsapp-mcp/prospect-api
cp ../whatsapp-bridge/.env.example .env  # use as a starting point
```

Then edit `.env` to add the prospect-api specific variables. At minimum:

```
PROSPECT_BIND_HOST=127.0.0.1
PROSPECT_BIND_PORT=8081

# Tokens — generate two distinct UUIDs.
PROSPECT_BRIDGE_AUTH_TOKEN=<uuidgen output 1>
PROSPECT_ADMIN_TOKEN=<uuidgen output 2>

# Vault CRM path
WHATSAPP_VAULT_CRM_PATH=/path/to/your/vault/CRM/

# Whatsapp bridge base URL (for sends in Phase B)
PROSPECT_BRIDGE_BASE_URL=http://127.0.0.1:8080

# DB key. macOS Keychain ACLs are per-binary; the bridge approved its own access.
# Easiest path: paste the key here once. Less convenient than Keychain auto-read,
# but avoids prompts when this binary first runs.
PROSPECT_DB_KEY=<run: security find-generic-password -s whatsapp-mcp -a default -w>
```

## Build

```bash
go build -o bin/prospect-api .
```

## Run

```bash
set -a; source .env; set +a
./bin/prospect-api
```

The service binds to `127.0.0.1:8081` by default. Verify health:

```bash
curl http://127.0.0.1:8081/healthz
# {"status":"ok","version":"0.1.0","timestamp":1777023123}
```

## Phase A endpoints

All endpoints take JSON, return JSON, and require `Authorization: Bearer <token>`.

- `POST /api/lookup-prospect` — bearer token. Existence + relationship + category enum.
  No content. No raw message bodies. Per-phone rate limited (5/min).

- `POST /api/pull-context` — bearer token. Server-verified identity required:
  the request's `phone` and `email` must match the same CRM record. Returns
  deterministic summary fields (recent topics via keyword frequency, last
  interaction time, message count). No raw messages.

- `POST /api/update-crm` — bearer token. Whitelist enforced server-side.
  Whitelist: email, phone, company, role, lastTopic, tagsAdd. Anything else
  silently dropped. Default-safe: only blank fields filled unless
  `forceOverwrite: true`.

- `POST /api/check-preset` — bearer token. Match arriving guest to a
  pre-warmed preset. Single-use; consuming the match marks the preset
  consumed.

- `POST /api/preset` — **admin token** (separate from bearer). Pre-warm a
  guest. Required: `guestName`, `context` (≤500 chars), `relationship`
  (`personal|professional|investor|media|press|speaking`), `expiresInDays`
  (1–30), and at least one of `guestEmail` / `guestPhone`.

## Rate limits

- Per-token (any endpoint): 60 req/min sliding window.
- Per-phone (lookup only, anti-enumeration): 5 req/min sliding window.

## Audit

Every request is logged to the prospect DB's `audit_log` table with:
endpoint, token kind, client IP, params metadata (no raw content), result
summary, duration, error string.

Query example:

```sql
SELECT
  datetime(ts, 'unixepoch') AS at,
  endpoint, token_kind, result_summary, duration_ms
FROM audit_log
ORDER BY ts DESC
LIMIT 50;
```

## Public exposure

This service binds loopback only. Public exposure is via Cloudflare Tunnel +
Access policy. See `SETUP-CLOUDFLARE.md` (next document).

## Threat model

See `SECURITY-PROSPECT.md` (next document).
