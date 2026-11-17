"""Shared test setup.

Tools write an audit record on every call, by default to
~/.claude/whatsapp-mcp/audit.log: the real log of whoever runs the tests.
Tests must never add fake entries there, so auditing is off unless a test
turns it on with its own path.
"""

from __future__ import annotations

import pytest

import main


@pytest.fixture(autouse=True)
def _no_real_audit_log(monkeypatch, tmp_path):
    monkeypatch.setattr(main, "AUDIT_LOG_ENABLED", False)
    monkeypatch.setattr(main, "AUDIT_LOG_PATH", str(tmp_path / "audit.log"))


@pytest.fixture(autouse=True)
def _no_unanswered_confirms_from_an_earlier_test():
    """confirm_send remembers drafts whose confirm raised; that must not cross tests."""
    main._unresolved_confirms.clear()
    yield
    main._unresolved_confirms.clear()
