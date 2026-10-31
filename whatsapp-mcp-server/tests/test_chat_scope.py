"""A workspace sees only its own chats (WHATSAPP_CHAT_SCOPE_FILE).

An app that sorts a person's WhatsApp chats into workspaces runs one MCP
server per workspace: the Personal one with a deny list (every chat but those
moved elsewhere), a company one with an allow list. These tests pin that a
scoped server never hands a session another workspace's chats, messages,
contacts, groups, drafts or media, under any JID the person has, and that an
unreadable or unclear scope file shows nothing rather than everything.
"""

from __future__ import annotations

import json

import pytest

import main

ONDE = "573001110000@s.whatsapp.net"
ONDE_LID = "71110000000000@lid"
FAMILY = "573002220000@s.whatsapp.net"
ONDE_GROUP = "120363000000000001@g.us"
# One person, two JIDs: the bridge's alias endpoint knows both.
FAMILIES = {ONDE: [ONDE, ONDE_LID], ONDE_LID: [ONDE_LID, ONDE]}


@pytest.fixture
def scope(tmp_path, monkeypatch):
    path = tmp_path / "scope.json"
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", str(path))
    monkeypatch.setattr(main, "_scoped_drafts", {})
    monkeypatch.setattr(main, "_scoped_messages", {})

    def write(data):
        path.write_text(json.dumps(data) if not isinstance(data, str) else data, encoding="utf-8")

    return write


@pytest.fixture
def bridge(monkeypatch):
    calls = []
    chats = [
        {"jid": ONDE_LID, "name": "Lucía", "aliases": [ONDE]},
        {"jid": FAMILY, "name": "Mamá"},
        {"jid": ONDE_GROUP, "name": "Onde equipo"},
    ]

    async def fake_get(path, params=None):
        calls.append(("GET", path, params))
        if path == "/api/aliases":
            jid = main._strip_device(params["jid"])
            return {"jid": params["jid"], "family": FAMILIES.get(jid, [jid])}
        if path == "/api/chats":
            rows = chats[params["offset"] : params["offset"] + params["limit"]]
            return {"chats": rows, "count": len(rows)}
        if path == "/api/contacts/search":
            return {"contacts": [{"jid": ONDE, "lid": ONDE_LID}, {"jid": FAMILY}], "count": 2}
        if path == "/api/groups":
            return {"groups": [{"jid": ONDE_GROUP, "name": "Onde equipo", "participant_count": 5}]}
        if path == "/api/messages":
            jid = params["chat_jid"]
            return {"messages": [{"id": f"m-{jid}", "content_text": "hola"}], "merged_jids": [jid]}
        if path == "/healthcheck":
            return {"status": "ok"}
        if path == "/api/status":
            return {"auth_state": "paired", "last_client_error": f"failed to decrypt message from {FAMILY}"}
        raise AssertionError(f"unexpected GET {path}")

    async def fake_post(path, body):
        calls.append(("POST", path, body))
        if path == "/api/sends":
            return {"draft_id": f"d-{body['recipient_jid']}"}
        return {"status": "ok"}

    monkeypatch.setattr(main, "_bridge_get", fake_get)
    monkeypatch.setattr(main, "_bridge_post", fake_post)
    return calls


def test_an_unscoped_server_sees_everything(monkeypatch):
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", "")
    assert main._chat_scope() is None
    assert main._family_allowed(None, [FAMILY])


@pytest.mark.parametrize(
    "content",
    ["missing", "{not json", '{"allow": {}, "deny": []}', '{"allow": [1, 2]}', '{"deny": "x"}', "[]"],
)
def test_an_unreadable_or_unclear_scope_file_shows_nothing(scope, monkeypatch, tmp_path, content):
    if content == "missing":
        monkeypatch.setattr(main, "CHAT_SCOPE_FILE", str(tmp_path / "missing.json"))
    else:
        scope(content)
    assert main._chat_scope() == ("allow", frozenset())


@pytest.mark.asyncio
async def test_a_company_workspace_lists_only_its_chats_contacts_and_groups(scope, bridge):
    scope({"allow": [ONDE, ONDE_GROUP]})
    chats = await main.list_chats()
    # The LID row is the same person as the allowed phone JID.
    assert [c["jid"] for c in chats["chats"]] == [ONDE_LID, ONDE_GROUP]
    contacts = await main.search_contacts("a")
    assert [c["jid"] for c in contacts["contacts"]] == [ONDE]
    groups = await main.search_groups("onde")
    assert [g["jid"] for g in groups["groups"]] == [ONDE_GROUP]


@pytest.mark.asyncio
async def test_a_chat_moved_out_of_personal_is_gone_under_every_jid(scope, bridge):
    scope({"deny": [ONDE, ONDE_GROUP]})
    chats = await main.list_chats()
    assert [c["jid"] for c in chats["chats"]] == [FAMILY]
    for jid in (ONDE, ONDE_LID, "573001110000:7@s.whatsapp.net"):
        with pytest.raises(RuntimeError, match="another workspace"):
            await main.list_messages(jid)
        with pytest.raises(RuntimeError, match="another workspace"):
            await main.send_message(jid, "hola")
        with pytest.raises(RuntimeError, match="another workspace"):
            await main.mark_chat_read(jid, [])
    assert not any(c[1] in ("/api/messages", "/api/sends") for c in bridge)


@pytest.mark.asyncio
async def test_messages_open_through_any_jid_of_an_allowed_chat(scope, bridge):
    scope({"allow": [ONDE]})
    result = await main.list_messages(ONDE_LID)
    assert result["messages"][0]["content_text"] == "hola"
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.list_messages(FAMILY)


@pytest.mark.asyncio
async def test_drafts_and_media_are_checked_again_after_a_chat_moves(scope, bridge):
    scope({"allow": [ONDE]})
    draft = await main.send_message(ONDE, "hola")
    await main.list_messages(ONDE)
    scope({"allow": []})
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.confirm_send(draft["draft_id"])
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.download_media("m-" + ONDE)
    assert not any(c[1].endswith("/confirm") or c[1] == "/api/media/download" for c in bridge)


@pytest.mark.asyncio
async def test_drafts_and_media_from_elsewhere_are_refused(scope, bridge):
    scope({"allow": [ONDE]})
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.confirm_send("d-made-elsewhere")
    with pytest.raises(RuntimeError, match="its own chats"):
        await main.download_media("m-unknown")
    draft = await main.send_message(ONDE, "hola")
    await main.confirm_send(draft["draft_id"])
    assert ("POST", f"/api/sends/{draft['draft_id']}/confirm", {}) in bridge


@pytest.mark.asyncio
async def test_a_workspace_whose_chats_are_not_the_newest_still_sees_them(scope, bridge):
    scope({"allow": [ONDE_GROUP]})
    page = await main.list_chats(limit=1)
    assert [c["jid"] for c in page["chats"]] == [ONDE_GROUP]
    assert (await main.list_chats(limit=1, offset=1))["chats"] == []


@pytest.mark.asyncio
async def test_healthcheck_drops_the_error_line_that_can_name_any_chat(scope, bridge):
    scope({"allow": [ONDE]})
    result = await main.healthcheck()
    assert "last_client_error" not in result["status_detail"]


@pytest.mark.asyncio
async def test_moving_a_chat_applies_on_the_next_call(scope, bridge):
    scope({"allow": [ONDE]})
    assert [c["jid"] for c in (await main.list_chats())["chats"]] == [ONDE_LID]
    scope({"allow": [ONDE, FAMILY]})
    assert [c["jid"] for c in (await main.list_chats())["chats"]] == [ONDE_LID, FAMILY]


@pytest.mark.asyncio
async def test_an_unscoped_server_remembers_nothing(monkeypatch, bridge):
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", "")
    monkeypatch.setattr(main, "_scoped_drafts", {})
    await main.send_message(FAMILY, "hola")
    assert main._scoped_drafts == {}
