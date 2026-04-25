# Changelog

## [0.1.0] — Phase A

Initial scaffold of the prospect-api sibling service.

### Added

- `POST /api/lookup-prospect` — low-trust recognition. Returns up to 5 matches
  with confidence scores (99 / 95 / 85 / 80 / 60 / 40 per the locked schema)
  plus the `categoryEnum` derived from a deterministic on-device categorizer.
  No raw message bodies. Per-phone rate limit: 5/min. Per-token rate limit: 60/min.

- `POST /api/pull-context` — high-trust context retrieval. Server-side identity
  verification: the request's phone and email must both match the same CRM
  record (or one of them plus a name match against record name/aliases).
  Returns deterministic summary fields: lastMessageAt, recentTopics, summary.
  No raw messages.

- `POST /api/update-crm` — server-enforced field whitelist. Allowed fields:
  `email`, `phone`, `company`, `role`, `lastTopic`, `tagsAdd`. Anything else
  is silently dropped. Default-safe behavior: only blank fields are filled.
  Pass `forceOverwrite: true` to replace existing values; logged with extra
  prominence.

- `POST /api/preset` — admin-token-only pre-warm. Required: `guestName`,
  `context` (≤500 chars), `relationship` (one of personal / professional /
  investor / media / press / speaking), `expiresInDays` (1–30), and at least
  one of `guestEmail` / `guestPhone`. Returns a single-use preset ID.

- `POST /api/check-preset` — bearer-token. Match arriving guest to a preset.
  Marks the preset consumed before returning so it cannot be replayed.

- `GET /healthz` — public, unauthenticated, basic up check for Cloudflare.

### Architecture

- Standalone Go module at `prospect-api/` rooted at `github.com/adelaidasofia/whatsapp-mcp/prospect-api`.
- Binds to 127.0.0.1 only; public exposure is via Cloudflare Tunnel.
- Reads the `whatsapp-bridge`'s encrypted SQLite DB (read-only) using the same
  Keychain-managed SQLCipher key. The DB key may be supplied via
  `PROSPECT_DB_KEY` env var to avoid the per-binary Keychain authorization
  prompt.
- Owns its own `presets.db` SQLite (unencrypted; low-sensitivity context blurbs)
  with `presets`, `audit_log`, and `wa_pings` tables.
- Loads vault CRM markdown asynchronously on startup so iCloud demand-paging
  does not block HTTP server bring-up.

### Categorizer

Deterministic, no LLM calls. Decision tree:

1. CRM `relationship` or tags hint → social or business_discussion.
2. Recent (≤14 days) message scan for scheduling keywords (English + Spanish)
   → scheduled_followup.
3. Total message count: 0 → unknown; <5 → intro_conversation; otherwise
   business_discussion (conservative default).

### Identity verification on pull-context

Per the panel security refinement (Howard Marks / Jackie Kennedy / Karpathy):
the bridge does NOT trust the agent's claim that identity has been verified.
The server fetches the matching CRM record and validates that ALL provided
identifiers match the SAME record. If phone and email both arrive, both must
match the same record. If only one identifier arrives, the optional name
must match the record's filename or aliases. Mismatch returns 403
`identity-mismatch`.

### Audit + rate limit

- Every request logs an entry to `audit_log` with endpoint, token kind,
  client IP (Cloudflare-aware), parameter metadata (no content), result
  summary, duration, and error string.
- Sliding-window rate limiters: 60/min per token, 5/min per phone for lookup.
- Cloudflare Access trust check (optional, env-gated): when
  `PROSPECT_REQUIRE_CF_ACCESS=true`, every request must include a
  `Cf-Access-Authenticated-User-Email` or `Cf-Access-Jwt-Assertion` header.

### Phase B preview

Phase B will add:

- `POST /api/get-negotiator-terms` (Sprint 10) reading from
  `⚙️ Meta/Negotiator Playbook.md`.
- `POST /api/notify-adelaida` (Sprint 8) sending via the whatsapp-bridge with
  a shared 10/hour budget across endpoints.
- `POST /api/relay-note` (Sprint 7) writing structured inbox files into
  `📮 Inbox/` and conditionally calling notify-adelaida.
- `POST /api/morning-digest` (Sprint 9) cron-triggered at the configured
  local hour by an internal goroutine.
