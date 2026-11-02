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

# Answered after the bridge has read the draft and found it still unsent.
REFUSED = [
    (400, "invalid recipient JID"),
    (403, "no_existing_conversation"),
    (404, "draft not found"),
    (410, "draft expired"),
    (429, "daily_send_limit"),
]
# 401 and 503 are answered BEFORE the bridge reads the draft, so they say nothing about
# whether an earlier confirm of the same draft sent it.
MAY_HAVE_GONE_OUT = [
    (401, "unauthorized"),
    (409, "draft already in state 'sent'"),
    (500, "confirm update failed"),
    (502, "send failed"),
    (503, "bridge not connected to WhatsApp"),
]
LOST_CONNECTION = [
    httpx.ReadError("connection reset by peer"),
    httpx.RemoteProtocolError("server disconnected without a response"),
    httpx.ReadTimeout("slow"),
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
@pytest.mark.parametrize("failure", [*LOST_CONNECTION, httpx.ConnectError("refused")])
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


def _first_call_then(first, then):
    """A bridge whose first confirm does `first` and whose later ones answer `then`."""
    calls = []

    def confirm(request):
        calls.append(request)
        if len(calls) == 1:
            if isinstance(first, Exception):
                raise first
            return first
        return then

    return confirm


@pytest.mark.asyncio
@pytest.mark.parametrize("failure", LOST_CONNECTION)
@pytest.mark.parametrize("status,error", [*REFUSED, *MAY_HAVE_GONE_OUT])
async def test_a_retry_after_a_failed_connection_never_reads_as_a_refusal(monkeypatch, failure, status, error):
    # The first confirm may have flipped the draft and sent it before the connection
    # went away. A refusal answered to the RETRY says only that the retry sent nothing:
    # a restarted bridge answers 503 before it reads the draft, and the daily limit is
    # checked before the draft is looked at again.
    _bridge(monkeypatch, _first_call_then(failure, httpx.Response(status, json={"error": error})))
    async with main.lifespan(None):
        with pytest.raises(RuntimeError):
            await main.confirm_send("d1")
        with pytest.raises(RuntimeError):
            await main.confirm_send("d1")


@pytest.mark.asyncio
async def test_a_retry_after_a_502_never_reads_as_a_refusal(monkeypatch):
    _bridge(monkeypatch, _first_call_then(httpx.Response(502, json={"error": "send failed"}), httpx.Response(429, json={"error": "daily_send_limit"})))
    async with main.lifespan(None):
        with pytest.raises(RuntimeError, match="bridge 502"):
            await main.confirm_send("d1")
        with pytest.raises(RuntimeError, match="bridge 429"):
            await main.confirm_send("d1")


@pytest.mark.asyncio
async def test_a_refusal_the_bridge_answered_leaves_the_draft_open_to_a_later_refusal(monkeypatch):
    # Nothing was sent by the first call, so a second refusal is still just a refusal.
    _bridge(monkeypatch, lambda request: httpx.Response(429, json={"error": "daily_send_limit"}))
    async with main.lifespan(None):
        first = await main.confirm_send("d1")
        second = await main.confirm_send("d1")
    assert first["sent"] is False and second["sent"] is False


@pytest.mark.asyncio
async def test_a_different_draft_is_not_affected_by_one_that_failed(monkeypatch):
    def confirm(request):
        if "/d1/" in request.url.path:
            raise httpx.ReadError("reset")
        return httpx.Response(429, json={"error": "daily_send_limit"})

    _bridge(monkeypatch, confirm)
    async with main.lifespan(None):
        with pytest.raises(RuntimeError):
            await main.confirm_send("d1")
        assert (await main.confirm_send("d2"))["sent"] is False


@pytest.mark.asyncio
async def test_the_record_of_unanswered_confirms_stays_bounded(monkeypatch):
    def confirm(request):
        raise httpx.ReadError("reset")

    _bridge(monkeypatch, confirm)
    async with main.lifespan(None):
        for n in range(main._UNRESOLVED_CONFIRMS_MAX + 40):
            with pytest.raises(RuntimeError):
                await main.confirm_send(f"draft-{n}")
    assert len(main._unresolved_confirms) <= main._UNRESOLVED_CONFIRMS_MAX


@pytest.mark.asyncio
async def test_the_bridge_is_reached_without_a_proxy(monkeypatch):
    # The bridge is a local program. A proxy in the environment could answer for it,
    # a 503 included, after the bridge had already sent the message, and would be
    # shown the whole message on the way.
    seen = {}
    real_client = httpx.AsyncClient

    def client_with_transport(**kwargs):
        seen.update(kwargs)
        return real_client(transport=httpx.MockTransport(lambda request: httpx.Response(200, json={"status": "ok"})), **kwargs)

    monkeypatch.setattr(main.httpx, "AsyncClient", client_with_transport)
    async with main.lifespan(None):
        pass
    assert seen.get("trust_env") is False
