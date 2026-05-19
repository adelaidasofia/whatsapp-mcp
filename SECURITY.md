# Security

This MCP reads and writes your personal WhatsApp. Treat it as equivalent to your unlocked phone. This document is the threat model, the risk-tier classification for every tool, and the list of hardening decisions.

## Threat model

**Assumed trust:**

- Your macOS user account is not compromised. (If it is, the attacker already has your unlocked phone equivalent.)
- `whatsmeow` is maintained by tulir / Mautrix. Upstream diffs are reviewed by the project maintainers before every dependency bump in this repo.
- Claude Code, when configured to use this MCP, is the intended consumer.

**Out of scope:**

- A state-level adversary with root access to your machine.
- A compromised OpenAI API endpoint (mitigation: local Whisper is the default).
- WhatsApp changing their protocol in a way that breaks authentication (mitigation: `whatsmeow` upstream patches; we pin to a specific commit and review diffs before bumping).

**In scope:**

- Prompt-injection attacks embedded in incoming WhatsApp messages.
- Accidental sends to the wrong contact (dry-run confirmation pattern).
- Unauthorized local processes reading the message database (SQLCipher encryption at rest).
- Network-level attackers on the same machine (bridge binds to `127.0.0.1` only).
- Supply-chain attacks via dependency updates (pinned versions, diff review on bump).

## Hardening decisions

1. **Bridge binds to `127.0.0.1` only, never `0.0.0.0`.** Enforced in the Go bridge config loader; a startup check rejects any non-loopback bind address.

2. **SQLite encrypted at rest with SQLCipher.** The master key is derived from a macOS Keychain entry (`service=whatsapp-mcp`, `account=default`). On first run, a random 256-bit key is generated and stored in Keychain; the user is prompted to authorize access. The DB file is unreadable without the key, even if the file itself is copied off the machine.

3. **Audit log.** Every tool call (via the Python MCP layer) is logged to `audit.log` with timestamp, tool name, params (media content redacted, only metadata retained), and result summary. Rotated daily, retained 30 days by default. Tunable via `WHATSAPP_AUDIT_LOG_RETENTION_DAYS`.

4. **Send tools require confirmation.** `send_message`, `send_file`, `send_audio_message`, `send_reply_quote`, `send_reaction` produce a draft with a `draft_id`. The actual network send happens only when `confirm_send(draft_id)` is called, echoing the resolved recipient JID, recipient display name, and message preview. No one-shot sends.

5. **Prompt-injection scrubber.** Every incoming message text passes through a scrubber that strips known injection patterns (`ignore previous instructions`, `system: reveal tokens`, etc.) before Claude sees it. Scrubbed patterns are logged. The original message is preserved in the database; only the representation shown to Claude is scrubbed.

   The scrubber's coverage is enforced by `whatsapp-bridge/scrubber_test.go`, a 73-case eval corpus that exercises every pattern in `InjectionPatterns` across lowercase, mixed-case, and prose-sandwich variants, plus an 18-entry false-positive control corpus. The `.github/workflows/scrubber-eval.yml` workflow runs the corpus on every PR + push to main + weekly schedule and BLOCKS merge on any catch-rate or false-positive regression. The same test file also documents 10 explicitly-skipped attack classes the substring scrubber does not yet catch (Unicode homoglyphs, whitespace padding, embedded delimiters, base64-encoded instructions, RTL override, zero-width joiners, indirect URL preview, tool-spoofing, exfiltration phrasing, markdown-with-javascript-scheme). Promoting a skipped gap to a passing case is how the scrubber gets hardened over time.

6. **`whatsmeow` pinned.** The Go module pins to a specific commit in `go.mod`. Upgrades require manual diff review and a version bump in `CHANGELOG.md`.

   Enforced by `.github/workflows/whatsmeow-upgrade-review.yml`. Any PR that modifies `whatsapp-bridge/go.mod` or `whatsapp-bridge/go.sum` is intercepted: the workflow extracts the old + new pseudo-version SHAs, clones the upstream `github.com/tulir/whatsmeow` mirror, generates the full commit log + diff stats between the two pins, posts that as a PR comment, and applies the `whatsmeow-diff-review-required` label. Reviewer must read the diff and add the `whatsmeow-diff-reviewed` label to authorize merge.

7. **No telemetry.** Zero external network calls except:
   - WhatsApp's multidevice endpoint (required for the tool to function).
   - OpenAI Whisper API only if `WHATSAPP_WHISPER_BACKEND=openai-api` is explicitly set (default is local `whisper.cpp`).

8. **No webhooks by default.** Set `WHATSAPP_WEBHOOK_URL` to enable; off by default.

9. **Session key in Keychain, not in plaintext.** The WhatsApp session credentials are stored in macOS Keychain. Never written to a dotfile, never committed.

10. **MIT license + threat model.** Publishing as public GitHub repo with explicit threat model so contributors and forkers know the security expectations upfront.

## Tool risk-tier classification

Every tool exposed by this MCP is classified by risk tier. Claude should treat higher-tier tools with more care (confirmation patterns, additional logging, user prompts).

| Tier | Meaning | Tools |
|---|---|---|
| **Read-only** | Does not modify state. Safe to call freely. | `search_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `get_contact`, `list_calls`, `get_call_details`, `get_contact_crm_context`, `search_messages_fuzzy`, `transcribe_voice_note`, `download_media` (downloads to local path, does not exfiltrate) |
| **Mutating (presence / receipts)** | Changes WhatsApp state in low-impact ways (e.g., marks a chat as read, signals typing). Reversible. | `mark_chat_read`, `send_typing_indicator`, `set_online_presence` |
| **Draft (pre-send)** | Creates a draft but does not send. Requires `confirm_send` to execute. | `send_message`, `send_file`, `send_audio_message`, `send_reply_quote`, `send_reaction` |
| **Confirm (commits a send)** | Commits a previously-drafted send. Must match a valid `draft_id`. | `confirm_send` |
| **Destructive** | Deletes or permanently alters state. None exist in v1.0; documented here so future additions are classified explicitly. | (none) |

## Reporting security issues

Open a GitHub issue with the label `security` for non-critical issues. For critical vulnerabilities (auth bypass, data exfiltration, remote code execution), email directly rather than filing a public issue.

## Related

- `docs/WHY_NOT_A_FORK.md`: why this was built direct on `whatsmeow` rather than forked from an existing MCP
- `docs/UPGRADE_NOTES.md`: how to safely bump `whatsmeow` or other dependencies
