# Phase B: Sprints 7-10

This document is the in-repo lock of the four endpoints that complete the AI
Concierge bridge integration. Phase A (lookup, pull-context, update-crm,
preset, check-preset) is shipped in `main`. Phase B implements the remaining
contract from the AI Executive Assistant PRD.

A fresh build session can build to spec from THIS DOCUMENT alone — no
external services need to be reachable.

## Build order (do not deviate)

The dependency chain runs **10 → 8 → 7 → 9**:

1. **Sprint 10** (`get-negotiator-terms`) — must exist before relay-note,
   because relay-note calls it for negotiable categories.
2. **Sprint 8** (`notify-adelaida`) — must exist before relay-note, because
   relay-note pings via this endpoint when urgency ≥ normal.
3. **Sprint 7** (`relay-note`) — depends on 10 + 8.
4. **Sprint 9** (`morning-digest`) — independent of the others; build last.

## Endpoint specs (locked)

### Sprint 7 — `POST /api/relay-note`

- **Auth:** Bearer.
- **Request:**
  ```json
  {
    "category": "<intent>",
    "guestName": "string",
    "guestEmail": "string?",
    "guestPhone": "string?",
    "guestContext": "string",
    "oneLineSummary": "string",
    "urgency": "low|normal|high",
    "reasoningBrief": {
      "audience": "string",
      "fit": "string",
      "upside": "string",
      "downside": "string",
      "lean": "accept|decline|consider"
    }
  }
  ```
- **Response:**
  ```json
  {
    "inboxFilePath": "absolute path",
    "crmUpdated": true,
    "whatsappPinged": true,
    "latencyMs": 0
  }
  ```
- **Side effects:**
  - Writes an inbox file (format below).
  - For urgency `normal` or `high`: calls `notify-adelaida` internally
    (subject to the shared WhatsApp ping budget).
  - For urgency `low`: writes the inbox file silently, no ping.
  - Optionally updates the CRM via the existing whitelist path. NEVER
    bypasses the whitelist.
- **Inbox file path:**
  `<vault root>/📮 Inbox/<YYYY-MM-DD>-<HHmm>-<category>-<guest-slug>.md`
  Collision: append `-<short-uuid>` before `.md`.
- **Inbox file frontmatter:**
  ```yaml
  ---
  type: inbox
  category: <intent>
  guestName: <string>
  guestEmail: <email?>
  guestPhone: <E.164?>
  arrivedAt: <ISO8601>
  urgency: low|normal|high
  crmRecordId: <string?>
  status: unread
  ---
  ```
- **Inbox file body sections (in order):**
  1. `## Summary` — the `oneLineSummary` from the request.
  2. `## Full conversation context` — `guestContext` from the request.
  3. `## Reasoning brief` — only when `reasoningBrief` is present in
     the request. Format: numbered 1–5 for who's asking, the ask, fit,
     upside/downside, lean. Use the request's structured values.
  4. `## Suggested response` — only for high-confidence cases (heuristic:
     match was found in CRM with confidence ≥ 95 AND lean = accept).
     Otherwise omit.
  5. `## Negotiation log` — only when category is in the negotiable set
     (see Sprint 10). Empty body initially; the concierge appends asker
     response and final disposition over time.

### Sprint 8 — `POST /api/notify-adelaida`

- **Auth:** Bearer.
- **Request:**
  ```json
  {
    "message": "string (≤500 chars)",
    "urgency": "low|normal|high",
    "deeplinkToInboxFile": "string?"
  }
  ```
- **Response:**
  ```json
  {
    "delivered": true,
    "deliveredAt": "<ISO8601?>",
    "latencyMs": 0
  }
  ```
- **Send path:** call the whatsapp-bridge HTTP API at
  `PROSPECT_BRIDGE_BASE_URL` (default `http://127.0.0.1:8080`).
  Use the two-step flow: `POST /api/sends` to create draft, then
  `POST /api/sends/{draft_id}/confirm` to commit. Recipient JID is
  the user's own number (configurable via `PROSPECT_SELF_JID` env;
  read from the bridge's `/api/status` if unset).
- **Shared WhatsApp ping budget:** 10 pings per hour TOTAL across
  `notify-adelaida` and `relay-note`. Implemented via a sliding window
  counter in the `wa_pings` table (already declared in `db.go`). Both
  endpoints insert into `wa_pings` after a successful send. Before
  sending, both check `SELECT COUNT(*) FROM wa_pings WHERE ts > now-3600`
  and refuse if ≥ 10. When refused, response is `delivered: false` with
  no error code (graceful queued state — the inbox file persists).
- **Quiet hours:** configurable via `PROSPECT_QUIET_HOURS_START` /
  `PROSPECT_QUIET_HOURS_END` (default `22:00` / `07:00`) in
  `PROSPECT_TIMEZONE`. When a request arrives during quiet hours:
  - `urgency: low` or `normal`: queue. Insert a row in a new
    `pending_pings` table with `deliver_at = next start-of-quiet-end`.
    A small scheduler goroutine in `main.go` ticks every minute,
    delivers anything past `deliver_at`, removes from table.
  - `urgency: high`: deliver immediately if `PROSPECT_QUIET_HOURS_OVERRIDE=true`,
    otherwise queue with `deliver_at = next start-of-quiet-end`.
  - The vault inbox note (written by relay-note) is unaffected by quiet
    hours; only the WhatsApp ping is delayed.

### Sprint 9 — `POST /api/morning-digest`

- **Auth:** Bearer.
- **Request:**
  ```json
  { "date": "<ISO8601 date?>" }
  ```
  Defaults to today.
- **Response:**
  ```json
  {
    "digestFilePath": "absolute path",
    "noteCount": 12,
    "latencyMs": 0
  }
  ```
- **Behavior:**
  - Reads all inbox files in `<vault root>/📮 Inbox/` whose
    `arrivedAt` falls in the previous 24 hours of the requested date.
  - Generates one line per inbox file: `- [<urgency>] <category>: <oneLineSummary> ([deeplink](path))`.
  - Trends section at top of digest:
    - Top intent of past 24h (most-common `category`).
    - Repeat-sender count (count of unique `guestEmail` or `guestPhone`
      that have ≥ 2 inbox files in the past 7 days).
    - Week-on-week note volume delta (current 24h count vs. same
      weekday last week's 24h count).
  - Output path: `<vault root>/📮 Inbox/Digest/<YYYY-MM-DD>-digest.md`.
  - All deterministic. No LLM call.
- **Internal scheduler:** started by `main.go`, ticks daily at the
  configured local hour (default 6am in `PROSPECT_TIMEZONE`). Uses
  `time.NewTimer` resetting every 24h. The scheduler hits the
  endpoint via internal HTTP (so the same audit + auth path runs)
  using the bearer token from env.

### Sprint 10 — `POST /api/get-negotiator-terms`

- **Auth:** Bearer.
- **Request:**
  ```json
  {
    "category": "speaking_ask|press_media|partnership|introduction_request|consulting_misfit",
    "guestContext": "string"
  }
  ```
- **Response:**
  ```json
  {
    "counterConditions": ["string", ...],
    "floorLanguage": "string?",
    "gracefulExit": "string",
    "playbookVersion": "<ISO8601 mtime of playbook file>"
  }
  ```
- **Behavior:**
  - Reads `<vault root>/⚙️ Meta/Negotiator Playbook.md` on every call.
    No cache beyond filesystem; updates are live.
  - Returns the section matching the request's `category`. If the
    section is missing, return 404 with code `playbook-section-missing`.
- **Playbook format (parser spec):**
  ```markdown
  ## <category>
  counter_conditions:
    - "string"
    - "string"
  floor_language: "string"
  graceful_exit: "string"
  special_notes: "optional notes the bridge ignores"
  ```
  Parse by `## ` heading + key/value (or YAML-style array for
  `counter_conditions`).
- **Scaffold playbook on Sprint 10 build:** create the playbook file
  if it does not exist, with all five categories present and TODO
  placeholders for each field. The user fills in the playbook over
  a separate sitting.

## Consulting-misfit special case (CRITICAL)

The `consulting_misfit` category in the playbook **must NOT offer the
free 20-minute AI Diagnostic**. The diagnostic is reserved for
above-floor qualified leads. Below-floor asks get one of:

1. Pointer to the next-cheaper published consulting tier.
2. Graceful decline + offer of free resources (Substack posts, GitHub
   repos, etc.).

The published consulting tiers are:

- $1,500 — Personal Install
- $3,500 — Team Install
- $10,000 — Custom engagement
- $4,000–5,000/month — Ongoing advisory

The concierge does NOT invent custom numbers. The Negotiator Playbook
scaffold for `consulting_misfit` should hard-code these tiers and the
"no diagnostic" rule.

## File layout

Following the established Phase A pattern:

- `prospect-api/handlers_relay.go` — Sprint 7 handler.
- `prospect-api/handlers_notify.go` — Sprint 8 handler.
- `prospect-api/handlers_digest.go` — Sprint 9 handler.
- `prospect-api/handlers_negotiator.go` — Sprint 10 handler.
- `prospect-api/inbox.go` — inbox file writer + guest-slug + collision-safe naming.
- `prospect-api/playbook.go` — Negotiator Playbook parser.
- `prospect-api/wa_budget.go` — shared 10/hr ping counter ops.
- `prospect-api/quiet_hours.go` — TZ-aware quiet window check + queue.
- `prospect-api/digest_scheduler.go` — daily goroutine for morning-digest.
- `prospect-api/main.go` — register new routes + start scheduler.
- `prospect-api/server.go` — wire the four new endpoints into routes.

## Schema deltas

The `wa_pings` table is already declared in `db.go`. New tables for Phase B:

```sql
CREATE TABLE IF NOT EXISTS pending_pings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  deliver_at INTEGER NOT NULL,
  message TEXT NOT NULL,
  urgency TEXT NOT NULL,
  deeplink TEXT,
  delivered INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_pending_pings_deliver_at ON pending_pings(deliver_at) WHERE delivered = 0;
```

Add this to `initPresetSchema` in `db.go` (idempotent via `IF NOT EXISTS`).

## Guardrails

- Bind stays loopback only.
- No external API calls (Anthropic, OpenAI). Bridge stays LLM-free.
- Audit log on every endpoint via `s.audit()`.
- Per-token + per-phone rate limit applies. Don't bypass.
- Path-prefix check on every vault write. Never write outside the
  configured vault root.
- relay-note's CRM update routes through the same whitelist as
  `/api/update-crm`. No bypass.
- Quiet hours: vault writes always immediate; only WhatsApp pings
  delayed.
- Personal-data scrub before every push (see prior commits for the
  grep command).

## Commit cadence

One commit per sprint. Multi-paragraph body. Match recent commit tone.

## Verification

After each sprint, smoke test the new endpoint with curl. After all
four are shipped, run an end-to-end test:

1. Concierge sends `POST /api/relay-note` with a `speaking_ask`
   category and `urgency: high`.
2. relay-note calls `get-negotiator-terms`, gets the speaking
   counter-conditions, includes them in the inbox file's
   "Negotiation log" body section.
3. relay-note calls `notify-adelaida` with a one-line summary.
4. notify-adelaida checks the shared budget, dispatches via the
   whatsapp-bridge.
5. The inbox file lands at the expected path.
6. The user's WhatsApp shows the ping.
7. At the configured local 6am, the morning-digest scheduler fires,
   the digest file lands with the 24h note + trends.

If all seven steps pass: Phase B is done. Tag v1.0.0.
