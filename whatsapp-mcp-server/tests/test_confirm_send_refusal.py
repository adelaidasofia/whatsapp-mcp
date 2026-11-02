"""confirm_send tells a send the bridge REFUSED from one that MAY HAVE GONE OUT.

An app reading confirm_send's result needs to know which it got. The bridge's send
rules (a daily limit, a first message to someone with no chat, the same text to a
second chat) answer 403 or 429 before the draft is flipped or anything is sent, and
a bridge that is down or not connected, or a draft that is missing or expired, never
reached WhatsApp either. Those come back as an ordinary answer saying nothing was
sent, so the app can say "not sent". Everything else keeps raising: a connection that
resets or times out after the bridge accepted the confirm, a 502 from the send
itself, a 409 (an earlier confirm may have sent it). The message may have gone out in
each of those, and an app that said "not sent" would have the person send it twice.
"""

from __future__ import annotations

import json

import httpx
import pytest

import main

REFUSED = [
    (400, "invalid recipient JID"),
    (401, "unauthorized"),
    (403, "no_existing_conversation"),
    (404, "draft not found"),
    (410, "draft expired"),
    (429, "daily_send_limit"),
    (503, "bridge not connected to WhatsApp"),
]
MAY_HAVE_GONE_OUT = [
    (409, "draft already in state 'sent'"),
    (500, "confirm update failed"),
    (502, "send failed"),
]


def _bridge(monkeypatch, confirm):
    """Run the server against a fake bridge whose confirm endpoint is `confirm`."""

    def handler(request):
        if request.url.path.endswith("/confirm"):
            return confirm(request)
        return httpx.Response(200, json={"status": "ok"})

    real_client = httpx.AsyncClient

    def client_with_transport(**kwargs):
        return real_client(transport=httpx.MockTransport(handler), **kwargs)

    monkeypatch.setattr(main.httpx, "AsyncClient", client_with_transport)


@pytest.mark.asyncio
@pytest.mark.parametrize("status,error", REFUSED)
async def test_a_refusal_the_bridge_answered_says_nothing_was_sent(monkeypatch, status, error):
    _bridge(monkeypatch, lambda request: httpx.Response(status, json={"error": error, "details": "why"}))
    async with main.lifespan(None):
        result = await main.confirm_send("d1")
    assert result["sent"] is False
    assert result["status"] == "refused"
    assert result["draft_id"] == "d1"
    assert f"bridge {status}: {error}" in result["error"], "the person and the model still read the bridge's reason"


@pytest.mark.asyncio
async def test_a_refusal_keeps_the_send_by_hand_link(monkeypatch):
    link = "https://wa.me/15555550100?text=hola"
    body = {"error": "daily_send_limit", "details": "at most 10 a day", "open_in_whatsapp": link}
    _bridge(monkeypatch, lambda request: httpx.Response(429, json=body))
    async with main.lifespan(None):
        result = await main.confirm_send("d1")
    assert link in result["error"]


@pytest.mark.asyncio
@pytest.mark.parametrize("status,error", MAY_HAVE_GONE_OUT)
async def test_an_answer_that_cannot_rule_out_a_send_still_raises(monkeypatch, status, error):
    _bridge(monkeypatch, lambda request: httpx.Response(status, json={"error": error}))
    async with main.lifespan(None):
        with pytest.raises(RuntimeError, match=f"bridge {status}"):
            await main.confirm_send("d1")


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "failure",
    [
        httpx.ReadError("connection reset by peer"),
        httpx.RemoteProtocolError("server disconnected without a response"),
        httpx.ReadTimeout("slow"),
        httpx.ConnectError("refused"),
    ],
)
async def test_a_connection_that_fails_still_raises(monkeypatch, failure):
    # The bridge may have accepted the confirm and sent the message before the
    # connection went away: nothing here can say it did not.
    def confirm(request):
        raise failure

    _bridge(monkeypatch, confirm)
    async with main.lifespan(None):
        with pytest.raises(RuntimeError):
            await main.confirm_send("d1")


@pytest.mark.asyncio
async def test_a_send_that_went_out_is_returned_as_before(monkeypatch):
    sent = {"draft_id": "d1", "status": "sent", "whatsapp_message_id": "3EB0"}
    _bridge(monkeypatch, lambda request: httpx.Response(200, json=sent))
    async with main.lifespan(None):
        assert await main.confirm_send("d1") == sent


@pytest.mark.asyncio
async def test_other_tools_keep_raising_a_refusal(monkeypatch):
    # Only confirm_send, the call that sends, answers a refusal as data.
    def handler(request):
        if request.url.path.endswith("/api/sends"):
            return httpx.Response(400, json={"error": "text required for send_type=text"})
        return httpx.Response(200, json={"status": "ok"})

    real_client = httpx.AsyncClient
    monkeypatch.setattr(main.httpx, "AsyncClient", lambda **kwargs: real_client(transport=httpx.MockTransport(handler), **kwargs))
    async with main.lifespan(None):
        with pytest.raises(RuntimeError, match="bridge 400"):
            await main.send_message("15555550100@s.whatsapp.net", "")


@pytest.mark.asyncio
async def test_a_refusal_is_audited_without_the_send_by_hand_link(monkeypatch, tmp_path):
    log_path = tmp_path / "audit.log"
    monkeypatch.setattr(main, "AUDIT_LOG_ENABLED", True)
    monkeypatch.setattr(main, "AUDIT_LOG_PATH", str(log_path))
    body = {"error": "daily_send_limit", "details": "x", "open_in_whatsapp": "https://wa.me/15555550100?text=secreto"}
    _bridge(monkeypatch, lambda request: httpx.Response(429, json=body))
    async with main.lifespan(None):
        await main.confirm_send("d1")
    entries = [json.loads(line) for line in log_path.read_text(encoding="utf-8").splitlines()]
    entry = [e for e in entries if e["tool"] == "confirm_send"][-1]
    assert entry["result_summary"] == "refused"
    assert "secreto" not in entry["error"] and "15555550100" not in entry["error"]
