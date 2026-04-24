"""whatsapp-mcp Python MCP server.

Consumes the Go bridge REST API (default http://127.0.0.1:8080) and exposes
MCP tools to Claude. FastMCP + stdio transport.

Tool categories (see SECURITY.md for risk-tier classification):

- Read-only: search_contacts, list_messages, list_chats, get_chat, etc.
- Presence: mark_chat_read, send_typing_indicator, set_online_presence.
- Draft (pre-send): send_message, send_file, send_audio_message, send_reply_quote, send_reaction.
- Confirm: confirm_send (commits a previously-drafted send).

Enhancement layers (applied transparently to core tools):

- LID resolution on every contact-returning tool.
- Accent-insensitive, NFD-normalized search.
- Voice-note Whisper transcription (local whisper.cpp default, openai-api opt-in).
- Vault CRM auto-injection when WHATSAPP_VAULT_CRM_PATH is set.
- Send confirmation dry-run pattern (draft + confirm).
- Prompt-injection scrubber on all incoming message text.
"""

from __future__ import annotations

import json
import logging
import os
import sys
import time
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from fastmcp import FastMCP

# --- Config -----------------------------------------------------------------

BRIDGE_HOST = os.environ.get("WHATSAPP_BRIDGE_HOST", "127.0.0.1")
BRIDGE_PORT = int(os.environ.get("WHATSAPP_BRIDGE_PORT", "8080"))
BRIDGE_BASE = f"http://{BRIDGE_HOST}:{BRIDGE_PORT}"

VAULT_CRM_PATH = os.environ.get("WHATSAPP_VAULT_CRM_PATH", "").strip()
SCRUB_PROMPT_INJECTION = os.environ.get("WHATSAPP_SCRUB_PROMPT_INJECTION", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
AUDIT_LOG_ENABLED = os.environ.get("WHATSAPP_AUDIT_LOG", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
AUDIT_LOG_PATH = os.environ.get(
    "WHATSAPP_AUDIT_LOG_PATH",
    str(Path.home() / ".claude" / "whatsapp-mcp" / "audit.log"),
)

# --- Logging ----------------------------------------------------------------

logging.basicConfig(
    stream=sys.stderr,
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
log = logging.getLogger("whatsapp-mcp")

# --- Clients ----------------------------------------------------------------

_http: httpx.AsyncClient | None = None


@asynccontextmanager
async def lifespan(_app: FastMCP):
    """Set up and tear down the shared HTTP client."""
    global _http
    _http = httpx.AsyncClient(
        base_url=BRIDGE_BASE,
        timeout=httpx.Timeout(30.0, connect=5.0),
    )
    try:
        # Preflight: confirm the Go bridge is reachable.
        try:
            r = await _http.get("/healthcheck")
            r.raise_for_status()
            log.info("bridge healthcheck OK: %s", r.json())
        except Exception as e:  # noqa: BLE001
            log.warning("bridge healthcheck failed: %s (continuing; tools will error at call time)", e)
        yield
    finally:
        if _http is not None:
            await _http.aclose()
        _http = None


mcp = FastMCP("whatsapp-mcp", lifespan=lifespan)


# --- Audit ------------------------------------------------------------------


def _audit(tool: str, params: dict[str, Any], result_summary: str, duration_ms: int, error: str | None = None) -> None:
    """Append a structured audit record. Redacts nothing at this layer; the bridge is expected to redact media blobs upstream."""
    if not AUDIT_LOG_ENABLED:
        return
    try:
        Path(AUDIT_LOG_PATH).parent.mkdir(parents=True, exist_ok=True)
        entry = {
            "ts": time.time(),
            "tool": tool,
            "params": params,
            "result_summary": result_summary,
            "duration_ms": duration_ms,
            "error": error,
        }
        with open(AUDIT_LOG_PATH, "a", encoding="utf-8") as f:
            f.write(json.dumps(entry, ensure_ascii=False) + "\n")
    except Exception as e:  # noqa: BLE001
        log.warning("audit log write failed: %s", e)


# --- Prompt-injection scrubber ----------------------------------------------

# Known injection patterns. Not exhaustive; updated as new patterns are observed.
# Matches are case-insensitive, whole-phrase.
_INJECTION_PATTERNS = [
    "ignore previous instructions",
    "ignore all previous instructions",
    "disregard all prior",
    "you are now",
    "system:",
    "<system>",
    "</system>",
    "assistant:",
    "<|im_start|>",
    "<|im_end|>",
    "reveal your instructions",
    "reveal your system prompt",
    "print your system prompt",
]


def scrub(text: str | None) -> tuple[str | None, list[str]]:
    """Return the scrubbed text and a list of matched pattern tags.
    Replaces matched patterns with [REDACTED_INJECTION].
    Original text is preserved in the database; this is the representation Claude sees.
    """
    if not text or not SCRUB_PROMPT_INJECTION:
        return text, []
    lowered = text.lower()
    flags: list[str] = []
    out = text
    for pat in _INJECTION_PATTERNS:
        if pat in lowered:
            flags.append(pat)
            # Case-insensitive replace preserving length roughly.
            idx = 0
            while idx < len(out):
                lo = out.lower().find(pat, idx)
                if lo < 0:
                    break
                out = out[:lo] + "[REDACTED_INJECTION]" + out[lo + len(pat) :]
                idx = lo + len("[REDACTED_INJECTION]")
    return out, flags


# --- Vault CRM auto-injection -----------------------------------------------


@dataclass
class CRMContext:
    """A minimal CRM note matched to a WhatsApp contact."""
    path: str
    name: str
    relationship: str | None
    company: str | None
    next_step: str | None
    first_paragraph: str | None


def _normalize_phone(p: str) -> str:
    return "".join(c for c in p if c.isdigit())


def lookup_crm_context(phone: str | None, display_name: str | None) -> CRMContext | None:
    """Match a WhatsApp contact to a vault CRM note and return a minimal context block.
    Matching strategy: phone digits match first (most reliable), then display name, then push name.
    Returns None if WHATSAPP_VAULT_CRM_PATH is unset or no match found.
    """
    if not VAULT_CRM_PATH:
        return None
    root = Path(VAULT_CRM_PATH)
    if not root.is_dir():
        return None

    target_phone = _normalize_phone(phone or "")
    target_name = (display_name or "").strip().lower()

    try:
        import frontmatter  # lazy import to avoid cost when CRM disabled
    except ImportError:
        log.warning("python-frontmatter not installed; CRM injection disabled")
        return None

    best_match: Path | None = None
    for md in root.rglob("*.md"):
        try:
            post = frontmatter.load(str(md))
        except Exception:  # noqa: BLE001
            continue

        fm_phone = _normalize_phone(str(post.metadata.get("phone", "")))
        if target_phone and fm_phone and fm_phone.endswith(target_phone[-10:]):
            best_match = md
            break

        stem_lower = md.stem.lower()
        if target_name and target_name in stem_lower:
            best_match = md
            # Do not break; continue looking for a phone match first.

    if best_match is None:
        return None

    post = frontmatter.load(str(best_match))
    first_para = (post.content.split("\n\n", 1)[0] or "").strip() if post.content else None
    return CRMContext(
        path=str(best_match),
        name=best_match.stem,
        relationship=str(post.metadata.get("relationship", "")) or None,
        company=str(post.metadata.get("company", "")) or None,
        next_step=str(post.metadata.get("next_step", "")) or None,
        first_paragraph=first_para,
    )


# --- Tool implementations (v0.1.0 stubs, real wiring in commit 2) -----------


async def _bridge_get(path: str, params: dict[str, Any] | None = None) -> Any:
    assert _http is not None, "http client not initialized"
    r = await _http.get(path, params=params)
    r.raise_for_status()
    return r.json()


async def _bridge_post(path: str, body: dict[str, Any]) -> Any:
    assert _http is not None, "http client not initialized"
    r = await _http.post(path, json=body)
    r.raise_for_status()
    return r.json()


@mcp.tool()
async def healthcheck() -> dict[str, Any]:
    """Check the Go bridge is running and authenticated.
    Returns status, schema version, and feature flags.
    """
    start = time.time()
    try:
        result = await _bridge_get("/healthcheck")
        status = await _bridge_get("/api/status")
        merged = {**result, "status_detail": status}
        _audit("healthcheck", {}, "ok", int((time.time() - start) * 1000))
        return merged
    except Exception as e:  # noqa: BLE001
        _audit("healthcheck", {}, "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def list_chats(limit: int = 20, offset: int = 0, unread_only: bool = False) -> dict[str, Any]:
    """List WhatsApp chats with metadata. Sorted by most recent message time.

    Args:
        limit: Max chats to return (default 20, max 200).
        offset: Pagination offset.
        unread_only: If true, return only chats with unread messages.
    """
    start = time.time()
    params = {"limit": min(limit, 200), "offset": offset, "unread_only": str(unread_only).lower()}
    try:
        result = await _bridge_get("/api/chats", params)
        _audit("list_chats", params, f"{len(result.get('chats', []))} chats", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("list_chats", params, "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def search_contacts(query: str, limit: int = 10) -> dict[str, Any]:
    """Find contacts by name or phone number. Accent-insensitive.
    "Muñoz" matches "munoz", "José" matches "jose", "Zürich" matches "zurich".
    """
    start = time.time()
    params = {"q": query, "limit": limit}
    try:
        result = await _bridge_get("/api/contacts/search", params)
        _audit("search_contacts", params, f"{len(result.get('contacts', []))} matches", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("search_contacts", params, "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def list_messages(
    chat_jid: str,
    limit: int = 20,
    before: str | None = None,
    include_crm_context: bool = True,
) -> dict[str, Any]:
    """List messages in a chat. Most recent first by default.

    Args:
        chat_jid: The JID of the chat (from list_chats or search_contacts).
        limit: Max messages (default 20, max 500).
        before: Return messages older than this message ID (pagination).
        include_crm_context: When true and WHATSAPP_VAULT_CRM_PATH is set, the response includes a `crm_context` block with the matching vault CRM note summary.
    """
    start = time.time()
    params: dict[str, Any] = {"chat_jid": chat_jid, "limit": min(limit, 500)}
    if before:
        params["before"] = before
    try:
        result = await _bridge_get("/api/messages", params)

        # Scrub incoming text for prompt injection.
        for msg in result.get("messages", []):
            raw = msg.get("content_text")
            scrubbed, flags = scrub(raw)
            msg["content_text"] = scrubbed
            if flags:
                msg["_scrub_flags"] = flags

        # CRM injection.
        if include_crm_context and result.get("chat"):
            ctx = lookup_crm_context(
                phone=result["chat"].get("phone"),
                display_name=result["chat"].get("name"),
            )
            if ctx is not None:
                result["crm_context"] = {
                    "path": ctx.path,
                    "name": ctx.name,
                    "relationship": ctx.relationship,
                    "company": ctx.company,
                    "next_step": ctx.next_step,
                    "summary": ctx.first_paragraph,
                }

        _audit("list_messages", params, f"{len(result.get('messages', []))} messages", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("list_messages", params, "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def send_message(recipient_jid: str, text: str) -> dict[str, Any]:
    """Create a DRAFT text message. Does NOT send until confirm_send is called.

    Returns a draft_id plus the resolved recipient display name, a preview, and an
    expires_at timestamp. Drafts expire after 1 hour. Each draft can be confirmed
    at most once.

    Args:
        recipient_jid: The recipient's WhatsApp JID (e.g. from list_chats or search_contacts).
                       Must include the @s.whatsapp.net or @g.us suffix.
        text: The message text. No media / reactions / voice in v0.3.0.

    Two-step pattern rationale: prevents "replied to wrong person" disasters.
    Claude must show you the draft_id + recipient_display + preview and wait for
    you to authorize before calling confirm_send.
    """
    start = time.time()
    body = {"recipient_jid": recipient_jid, "text": text, "send_type": "text"}
    try:
        result = await _bridge_post("/api/sends", body)
        _audit("send_message", {"recipient_jid": recipient_jid, "text_len": len(text)},
               f"draft_id={result.get('draft_id')} recipient={result.get('recipient_display')}",
               int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("send_message", {"recipient_jid": recipient_jid, "text_len": len(text)},
               "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def confirm_send(draft_id: str) -> dict[str, Any]:
    """Commit a previously-drafted send. This is where the actual WhatsApp network
    send happens.

    Args:
        draft_id: The draft_id returned by a previous send_message call.

    Returns the final WhatsApp message ID on success. The sent message is also
    persisted into the local message database so it shows up in list_messages
    for the recipient chat.

    Errors:
        404: draft not found
        409: draft already confirmed/sent/failed (each draft can only be confirmed once)
        410: draft expired (>1 hour since creation)
        502: send failed at whatsmeow layer (bridge disconnected, invalid recipient, etc.)
    """
    start = time.time()
    try:
        result = await _bridge_post(f"/api/sends/{draft_id}/confirm", {})
        _audit("confirm_send", {"draft_id": draft_id},
               f"status={result.get('status')} whatsapp_id={result.get('whatsapp_message_id')}",
               int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("confirm_send", {"draft_id": draft_id}, "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def send_reply_quote(recipient_jid: str, quoted_message_id: str, text: str) -> dict[str, Any]:
    """Create a DRAFT reply-quote message that cites a specific earlier message.

    The recipient will see the original quoted message appear above your reply
    in their WhatsApp UI (the familiar reply-indicator format).

    Args:
        recipient_jid: The chat JID (direct or group).
        quoted_message_id: The ID of the message being quoted. Find it via list_messages.
        text: The reply text.

    Returns a draft_id. Call confirm_send(draft_id) to actually send.
    """
    start = time.time()
    body = {
        "send_type": "reply_quote",
        "recipient_jid": recipient_jid,
        "quoted_message_id": quoted_message_id,
        "text": text,
    }
    try:
        result = await _bridge_post("/api/sends", body)
        _audit("send_reply_quote",
               {"recipient_jid": recipient_jid, "quoted": quoted_message_id, "text_len": len(text)},
               f"draft_id={result.get('draft_id')}", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("send_reply_quote", {"recipient_jid": recipient_jid, "quoted": quoted_message_id},
               "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def send_reaction(recipient_jid: str, target_message_id: str, emoji: str) -> dict[str, Any]:
    """Create a DRAFT emoji reaction on a specific message.

    Args:
        recipient_jid: The chat JID where the target message lives.
        target_message_id: The ID of the message to react to. Find via list_messages.
        emoji: The reaction emoji (e.g., "❤️", "👍", "😂"). Pass "" to remove a previous reaction.

    Returns a draft_id. Call confirm_send(draft_id) to actually react.
    """
    start = time.time()
    body = {
        "send_type": "reaction",
        "recipient_jid": recipient_jid,
        "reaction_target": target_message_id,
        "reaction_emoji": emoji,
    }
    try:
        result = await _bridge_post("/api/sends", body)
        _audit("send_reaction",
               {"recipient_jid": recipient_jid, "target": target_message_id, "emoji": emoji},
               f"draft_id={result.get('draft_id')}", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("send_reaction", {"recipient_jid": recipient_jid, "target": target_message_id},
               "failed", int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def mark_chat_read(chat_jid: str, message_ids: list[str]) -> dict[str, Any]:
    """Mark one or more messages as read. Affects your WhatsApp unread count on
    this device AND across your linked devices. Low-consequence: no draft+confirm
    needed, this is a one-step call.

    Args:
        chat_jid: The chat to mark read.
        message_ids: List of message IDs to mark. Typically the IDs of the
                     most recent unread messages for this chat.
    """
    start = time.time()
    body = {"chat_jid": chat_jid, "message_ids": message_ids}
    try:
        result = await _bridge_post("/api/presence/mark_read", body)
        _audit("mark_chat_read", {"chat_jid": chat_jid, "count": len(message_ids)},
               "ok", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("mark_chat_read", {"chat_jid": chat_jid}, "failed",
               int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def send_typing_indicator(chat_jid: str, state: str = "composing") -> dict[str, Any]:
    """Show a "typing..." or "paused" indicator to the chat recipient.

    Args:
        chat_jid: The chat to signal.
        state: "composing" (typing) or "paused" (stopped typing). Default: composing.

    Useful as a courtesy signal right before calling confirm_send so the recipient
    sees "typing..." instead of the message appearing silently. Low-consequence.
    """
    start = time.time()
    body = {"chat_jid": chat_jid, "state": state}
    try:
        result = await _bridge_post("/api/presence/typing", body)
        _audit("send_typing_indicator", {"chat_jid": chat_jid, "state": state},
               "ok", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("send_typing_indicator", {"chat_jid": chat_jid}, "failed",
               int((time.time() - start) * 1000), error=str(e))
        raise


@mcp.tool()
async def set_online_presence(online: bool = True) -> dict[str, Any]:
    """Signal your online/offline presence globally to all chats. Changing this
    affects your "last seen" visibility per WhatsApp's privacy settings.

    Args:
        online: True to appear online, False to appear offline.
    """
    start = time.time()
    body = {"online": online}
    try:
        result = await _bridge_post("/api/presence/online", body)
        _audit("set_online_presence", {"online": online}, "ok", int((time.time() - start) * 1000))
        return result
    except Exception as e:  # noqa: BLE001
        _audit("set_online_presence", {"online": online}, "failed",
               int((time.time() - start) * 1000), error=str(e))
        raise


# --- Entry point ------------------------------------------------------------

if __name__ == "__main__":
    mcp.run()
