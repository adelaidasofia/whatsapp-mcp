"""The bridge-unreachable message is the most-read error this project has.

A fresh install lands in exactly one state more often than any other: the MCP
server is registered, its tools appear in Claude, and the Go bridge -- a
separate program -- is not running. httpx raises ConnectError, which is NOT an
httpx.HTTPStatusError, so before this it slipped past _bridge_error and
surfaced as:

    Error calling tool 'list_chats': All connection attempts failed

which names neither the bridge, the port, nor the fix. Both the person and the
model then guess, usually at the MCP config -- the one part that was already
correct.

These tests pin the recovery information into the message, because it is the
only thing standing between a student and a dead end.
"""

from __future__ import annotations

import httpx
import pytest

from main import (
    BRIDGE_BASE,
    _bridge_timeout,
    _bridge_unreachable,
    _transport_error,
)


def test_unreachable_message_names_the_bridge_and_the_address():
    msg = str(_bridge_unreachable(httpx.ConnectError("nope")))
    assert BRIDGE_BASE in msg, "must say WHERE it tried to connect"
    assert "bridge" in msg.lower(), "must name the bridge as the missing piece"
    assert "separate program" in msg, "must explain the bridge is not this process"


def test_unreachable_message_tells_them_how_to_start_it():
    msg = str(_bridge_unreachable(httpx.ConnectError("nope")))
    assert "whatsapp-bridge" in msg, "must include the command to run"
    assert "install-bridge-autostart" in msg, "must point at the autostart fix"


def test_unreachable_message_warns_the_qr_cannot_be_relayed():
    # The other repeat failure: an agent tries to relay a pairing code that
    # expires in ~20s. Saying so here catches it at the moment of confusion.
    msg = str(_bridge_unreachable(httpx.ConnectError("nope")))
    assert "QR" in msg
    assert "cannot be relayed" in msg


def test_unreachable_message_names_the_env_vars_to_change_the_port():
    msg = str(_bridge_unreachable(httpx.ConnectError("nope")))
    assert "WHATSAPP_BRIDGE_PORT" in msg
    assert "WHATSAPP_BRIDGE_HOST" in msg


@pytest.mark.parametrize(
    "exc",
    [
        httpx.ConnectError("refused"),
        httpx.ConnectTimeout("timed out connecting"),
    ],
)
def test_connect_failures_classify_as_not_running(exc):
    msg = str(_transport_error(exc))
    assert "no WhatsApp tool works until it is running" in msg


def test_read_timeout_is_not_reported_as_not_running():
    # A bridge that accepted the connection and then went quiet is a DIFFERENT
    # problem from one that was never started, and telling someone to start an
    # already-running bridge sends them the wrong way.
    msg = str(_transport_error(httpx.ReadTimeout("slow")))
    assert "accepted the connection" in msg
    assert "did not answer in time" in msg


def test_timeout_message_points_at_the_log():
    msg = str(_bridge_timeout(httpx.ReadTimeout("slow")))
    assert "log" in msg.lower()


def test_transport_error_returns_runtimeerror_not_raises():
    # The call sites do `raise _transport_error(e) from e`, so this must RETURN
    # an exception rather than raise one, or the original context is lost.
    got = _transport_error(httpx.ConnectError("nope"))
    assert isinstance(got, RuntimeError)
