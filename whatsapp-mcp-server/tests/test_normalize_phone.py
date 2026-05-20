"""Unit tests for _normalize_phone in whatsapp-mcp-server/main.py.

Strips every non-digit character. Used for loose phone matching against
frontmatter phone fields stored in various formats (+1-555-1234567,
(555) 123-4567, etc.).

All test numbers use the US 555-prefixed reserved-for-fiction range so no
real number is referenced.
"""

from __future__ import annotations

import pytest

from main import _normalize_phone


@pytest.mark.parametrize(
    "raw,expected",
    [
        ("+1 555 1234567", "15551234567"),
        ("+1-555-123-4567", "15551234567"),
        ("+1 (555) 987-6543", "15559876543"),
        ("555.123.4567", "5551234567"),
        ("5551234567", "5551234567"),
    ],
)
def test_strips_separators_and_plus(raw, expected):
    """Spaces, dashes, parens, dots, leading + all stripped to digits only."""
    assert _normalize_phone(raw) == expected


def test_empty_string_returns_empty():
    assert _normalize_phone("") == ""


def test_letters_dropped_silently():
    """Letters (e.g. accidental paste of full vCard) drop to digits only."""
    assert _normalize_phone("abc123def456") == "123456"


def test_all_separators_no_digits():
    """Phone field of nothing-but-formatting returns empty (matchable as None)."""
    assert _normalize_phone("+-() ") == ""


def test_return_type_is_string():
    """Defensive: always returns str, never None / bytes."""
    assert isinstance(_normalize_phone("+1 555"), str)
    assert isinstance(_normalize_phone(""), str)
