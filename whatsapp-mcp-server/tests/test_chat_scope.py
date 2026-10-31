"""A workspace sees only its own chats (WHATSAPP_CHAT_SCOPE_FILE).

An app that sorts a person's WhatsApp chats into workspaces runs one MCP
server per workspace: the Personal one with a deny list (every chat but those
moved elsewhere), a company one with an allow list. These tests pin that a
scoped server never hands a session another workspace's chats, messages,
contacts, groups, drafts or media, and that an unreadable scope file shows
nothing rather than everything.
"""

from __future__ import annotations

import json

import pytest

import main

ONDE = "573001110000@s.whatsapp.net"
ONDE_LID = "71110000000000@lid"
FAMILY = "573002220000@s.whatsapp.net"
ONDE_GROUP = "120363000000000001@g.us"


@pytest.fixture
def scope(tmp_path, monkeypatch):
    path = tmp_path / "scope.json"
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", str(path))
    monkeypatch.setattr(main, "_scoped_drafts", set())
    monkeypatch.setattr(main, "_scoped_messages", set())

    def write(data):
        path.write_text(json.dumps(data), encoding="utf-8")

    return write


@pytest.fixture
def bridge(monkeypatch):
    calls = []

    async def fake_get(path, params=None):
        calls.append(("GET", path, params))
        if path == "/api/chats":
            return {"chats": [{"jid": ONDE}, {"jid": FAMILY}, {"jid": ONDE_GROUP}], "count": 3}
        if path == "/api/contacts/search":
            return {"contacts": [{"jid": ONDE, "lid": ONDE_LID}, {"jid": FAMILY}], "count": 2}
        if path == "/api/groups":
            return {"groups": [{"jid": ONDE_GROUP, "name": "Onde equipo", "participant_count": 5}]}
        if path == "/api/messages":
            jid = params["chat_jid"]
            merged = [ONDE, ONDE_LID] if jid in (ONDE, ONDE_LID) else [jid]
            return {"messages": [{"id": f"m-{jid}", "content_text": "hola"}], "merged_jids": merged}
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
    assert main._in_scope(ONDE) and main._in_scope(FAMILY)


def test_an_unreadable_scope_file_shows_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", str(tmp_path / "missing.json"))
    assert not main._in_scope(ONDE)
    (tmp_path / "bad.json").write_text("{not json", encoding="utf-8")
    monkeypatch.setattr(main, "CHAT_SCOPE_FILE", str(tmp_path / "bad.json"))
    assert not main._in_scope(FAMILY)


@pytest.mark.asyncio
async def test_a_company_workspace_lists_only_its_chats_contacts_and_groups(scope, bridge):
    scope({"allow": [ONDE, ONDE_GROUP]})
    chats = await main.list_chats()
    assert [c["jid"] for c in chats["chats"]] == [ONDE, ONDE_GROUP]
    contacts = await main.search_contacts("a")
    assert [c["jid"] for c in contacts["contacts"]] == [ONDE]
    groups = await main.search_groups("onde")
    assert [g["jid"] for g in groups["groups"]] == [ONDE_GROUP]


@pytest.mark.asyncio
async def test_personal_keeps_every_chat_but_the_ones_moved_out(scope, bridge):
    scope({"deny": [ONDE, ONDE_GROUP]})
    chats = await main.list_chats()
    assert [c["jid"] for c in chats["chats"]] == [FAMILY]
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.list_messages(ONDE)
    # The same person through their other JID is still the moved chat.
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.list_messages(ONDE_LID)


@pytest.mark.asyncio
async def test_messages_open_through_any_jid_the_bridge_merged(scope, bridge):
    scope({"allow": [ONDE]})
    result = await main.list_messages(ONDE_LID)
    assert result["messages"][0]["content_text"] == "hola"
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.list_messages(FAMILY)


@pytest.mark.asyncio
async def test_sends_stay_inside_the_workspace(scope, bridge):
    scope({"allow": [ONDE]})
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.send_message(FAMILY, "hola")
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.send_reaction(FAMILY, "m1", "👍")
    draft = await main.send_message(ONDE, "hola")
    await main.confirm_send(draft["draft_id"])
    # A draft another workspace's server made cannot be confirmed here.
    with pytest.raises(RuntimeError, match="another workspace"):
        await main.confirm_send("d-made-elsewhere")
    assert not any(c[1] == "/api/sends/d-made-elsewhere/confirm" for c in bridge)


@pytest.mark.asyncio
async def test_media_only_from_messages_this_workspace_showed(scope, bridge):
    scope({"allow": [ONDE]})
    with pytest.raises(RuntimeError, match="its own chats"):
        await main.download_media("m-" + FAMILY)
    await main.list_messages(ONDE)
    await main.download_media("m-" + ONDE)
    assert ("POST", "/api/media/download", {"message_id": "m-" + ONDE}) in bridge


@pytest.mark.asyncio
async def test_moving_a_chat_applies_on_the_next_call(scope, bridge):
    scope({"allow": [ONDE]})
    assert [c["jid"] for c in (await main.list_chats())["chats"]] == [ONDE]
    scope({"allow": [ONDE, FAMILY]})
    assert [c["jid"] for c in (await main.list_chats())["chats"]] == [ONDE, FAMILY]
