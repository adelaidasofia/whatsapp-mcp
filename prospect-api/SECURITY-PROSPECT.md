# prospect-api Threat Model

The `prospect-api` is the public-facing layer of the whatsapp-mcp ecosystem.
It sits behind Cloudflare Tunnel and is callable by an authorized AI Concierge
on behalf of arriving guests. Its threat profile is materially different from
the local-only `whatsapp-bridge`.

## Trust assumptions

- The user's macOS account is not compromised.
- Cloudflare Tunnel is not compromised; Cloudflare Access policy correctly
  filters origin IPs to the AI Concierge's known egress.
- The AI Concierge's bearer token is held server-side only (never exposed to
  the visitor's browser).

## In scope

1. **Spearphishing via lookup enumeration.** A malicious visitor types various
   phone numbers / emails into the concierge to map who is in the user's
   circle and what categories of relationships exist.
2. **Identity spoofing on pull-context.** A malicious visitor or a compromised
   AI Concierge agent provides a name + email guessed from public sources to
   pull personal context.
3. **CRM corruption via update-crm.** The agent or visitor passes "corrected"
   field values that quietly overwrite curated CRM data.
4. **Preset replay.** A leaked preset token is reused multiple times.
5. **Rate-based attacks.** A misbehaving agent (or runaway prompt) hammers the
   API.
6. **Audit-log tampering.** An attacker who gains write access tries to remove
   their footprint.

## Mitigations

### 1. Spearphishing on lookup

- Lookup returns the `categoryEnum` only. Specifically: `business_discussion`,
  `scheduled_followup`, `social`, `intro_conversation`, `unknown`.
- Lookup never returns: `lastTopic` text, `notes` content, raw message bodies,
  message timestamps, calendar event details, or any string the user wrote.
- Per-phone rate limit (5/min sliding window) prevents an attacker from
  enumerating a phonebook in any reasonable time window.
- The categorizer is deterministic and on-device; no LLM call leaks anything
  to a third party (Anthropic, OpenAI, etc.).

### 2. Identity spoofing on pull-context

- Server-side identity verification (NOT agent-asserted). The bridge fetches
  the matching CRM record and validates that all identifiers in the request
  match the SAME record. Phone + email both required to match if both
  provided. Mismatch returns 403 `identity-mismatch`.
- The agent is treated as untrusted because it could be prompt-injected, have
  a code bug, or be compromised. The bridge enforces the gate regardless.
- Pull-context returns deterministic summaries (recent topics via keyword
  frequency, last interaction time, message count). It never returns raw
  message bodies; the agent cannot exfiltrate content even with a verified
  identity.

### 3. CRM corruption

- Server-enforced field whitelist (`email`, `phone`, `company`, `role`,
  `lastTopic`, `tagsAdd`). Any other field is silently dropped and logged.
  Emotional / vulnerable / personal / therapeutic content cannot enter the
  CRM via this path regardless of what the agent sends.
- Default-safe write behavior: only blank fields are filled. To overwrite
  an existing value, the request must explicitly set `forceOverwrite: true`,
  which logs at higher prominence.
- Body content (free-form prose under the YAML frontmatter) is never modified
  by this endpoint; only structured frontmatter fields.

### 4. Preset replay

- Every preset is single-use. The first `/api/check-preset` that matches the
  preset marks it `consumed_at` in the same transaction; subsequent matches
  return `matched: false`.
- Presets have a TTL (1–30 days) chosen at creation time. Expired presets are
  unmatched.
- Presets require admin-token auth (separate from bearer); admin token must
  not be reused across services.

### 5. Rate-based attacks

- 60 req/min per token (sliding window) on every endpoint.
- 5 req/min per phone on lookup (anti-enumeration).
- Future: shared 10/hour WhatsApp ping budget across `relay-note` and
  `notify-adelaida` (Sprint 7+8 in Phase B).

### 6. Audit log

- Audit entries are written to the prospect DB on every request (Sqlite
  `audit_log` table). They include endpoint, token kind, client IP
  (Cloudflare-aware), parameter metadata, result summary, duration, error.
- The audit DB is local; macOS FileVault protects it at rest.
- A SQL injection that wipes the table would itself be a high-severity
  finding — the same DB is used for presets and would be noticed
  immediately.

## Network posture

- Bind: `127.0.0.1` only, validated at startup. Any non-loopback bind
  address aborts startup.
- Public exposure: Cloudflare Tunnel only. The Cloudflare Access policy
  is configured to permit only the AI Concierge's egress IPs (or, more
  robustly, a service-token JWT).
- No outbound calls to third-party services from the lookup, pull-context,
  update-crm, or preset paths. Phase B's relay-note will outbound to the
  whatsapp-bridge HTTP API on `127.0.0.1:8080` only.

## Optional Cloudflare Access trust check

Setting `PROSPECT_REQUIRE_CF_ACCESS=true` requires every request to include
either `Cf-Access-Authenticated-User-Email` or `Cf-Access-Jwt-Assertion`.
Cloudflare Access injects these on requests it has authenticated. Without
the header, the request is rejected with 403 even if the bearer token is
valid. Defense-in-depth.

## Reporting

Open a GitHub issue with the `security` label. For high-severity findings
(auth bypass, identity verification bypass, audit log tampering), email
the maintainer directly rather than posting publicly.
