"""Unit tests for lookup_crm_context in whatsapp-mcp-server/main.py.

The CRM lookup auto-injects matching vault notes into chat tool responses.
Match strategy (in order): phone digits, then display name in filename stem.
Phone always wins when both could match.

Tests use a tempdir-backed fake CRM folder with markdown + frontmatter, so no
real vault content touches the test suite. All names + phone numbers are
fully fictional (US 555-prefixed reserved-for-fiction range). `monkeypatch`
overrides the module-level VAULT_CRM_PATH constant since the function reads
it eagerly at module load.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from main import lookup_crm_context


# --- Fixtures -----------------------------------------------------------------


@pytest.fixture
def crm_dir(tmp_path: Path, monkeypatch) -> Path:
    """Tempdir set as VAULT_CRM_PATH. Empty until tests write notes into it."""
    d = tmp_path / "crm"
    d.mkdir()
    monkeypatch.setattr("main.VAULT_CRM_PATH", str(d))
    return d


def write_crm_note(folder: Path, name: str, **frontmatter) -> Path:
    """Write a markdown file with frontmatter for a fake CRM note. Returns path."""
    lines = ["---"]
    for k, v in frontmatter.items():
        lines.append(f"{k}: {v}")
    lines.append("---")
    lines.append("")
    lines.append("First paragraph of the CRM note.")
    lines.append("")
    lines.append("Second paragraph that should not appear in summary.")
    p = folder / f"{name}.md"
    p.write_text("\n".join(lines), encoding="utf-8")
    return p


# --- Path-unset / missing-dir cases -------------------------------------------


def test_returns_none_when_path_unset(monkeypatch):
    """No VAULT_CRM_PATH set: return None (CRM injection disabled)."""
    monkeypatch.setattr("main.VAULT_CRM_PATH", "")
    got = lookup_crm_context(phone="+15551234567", display_name="Alice")
    assert got is None


def test_returns_none_when_path_does_not_exist(monkeypatch):
    """VAULT_CRM_PATH set but the directory doesn't exist: None."""
    monkeypatch.setattr("main.VAULT_CRM_PATH", "/this/path/definitely/does/not/exist")
    got = lookup_crm_context(phone="+15551234567", display_name="Alice")
    assert got is None


# --- Phone match cases --------------------------------------------------------


def test_match_by_phone_digits(crm_dir):
    """Phone digits in frontmatter match incoming phone (digit-only equality)."""
    write_crm_note(
        crm_dir,
        "Alice Tester",
        phone="+15551234567",
        relationship="client",
        company="Example Corp",
    )
    got = lookup_crm_context(phone="+1 (555) 123-4567", display_name="Unrelated")
    assert got is not None
    assert got.name == "Alice Tester"
    assert got.relationship == "client"
    assert got.company == "Example Corp"
    assert got.first_paragraph == "First paragraph of the CRM note."


def test_phone_match_ignores_separators(crm_dir):
    """Match works regardless of spaces, dashes, parentheses on either side."""
    write_crm_note(crm_dir, "Bob Tester", phone="+1-555-987-6543")
    got = lookup_crm_context(phone="15559876543", display_name=None)
    assert got is not None
    assert got.name == "Bob Tester"


def test_phone_match_last_10_digits(crm_dir):
    """The matcher compares the last 10 digits (E.164 country-code-tolerant)."""
    write_crm_note(crm_dir, "Carol Example", phone="5551234567")
    got = lookup_crm_context(phone="+15551234567", display_name=None)
    assert got is not None
    assert got.name == "Carol Example"


# --- Name match cases ---------------------------------------------------------


def test_match_by_display_name_when_no_phone(crm_dir):
    """No phone provided, but display name appears in filename: return that file."""
    write_crm_note(crm_dir, "Carol Example", company="Example Corp")
    got = lookup_crm_context(phone=None, display_name="Carol")
    assert got is not None
    assert got.name == "Carol Example"
    assert got.company == "Example Corp"


def test_name_match_case_insensitive(crm_dir):
    """Filename stem comparison is case-insensitive."""
    write_crm_note(crm_dir, "Dora Example", relationship="contact")
    got = lookup_crm_context(phone=None, display_name="dora")
    assert got is not None
    assert got.name == "Dora Example"


def test_phone_match_wins_over_name(crm_dir):
    """When both phone and name could match, phone wins."""
    write_crm_note(crm_dir, "Carol Acme", phone="+15551234567", company="Acme Corp")
    write_crm_note(crm_dir, "Carol Example", company="Example Corp")
    got = lookup_crm_context(phone="+15551234567", display_name="Carol")
    assert got is not None
    # Phone-matched file wins, even though name also matches the other one.
    assert got.company == "Acme Corp"


# --- No-match cases -----------------------------------------------------------


def test_no_match_returns_none(crm_dir):
    """Neither phone nor name match anything: None."""
    write_crm_note(crm_dir, "Alice Tester", phone="+15551234567")
    got = lookup_crm_context(phone="+15559999999", display_name="Stranger")
    assert got is None


def test_empty_crm_dir_returns_none(crm_dir):
    """Empty CRM dir: None even with valid inputs."""
    got = lookup_crm_context(phone="+15551234567", display_name="Alice")
    assert got is None


# --- Frontmatter robustness ---------------------------------------------------


def test_missing_frontmatter_fields_render_as_none(crm_dir):
    """Matched file with minimal frontmatter: missing fields render as None."""
    write_crm_note(crm_dir, "Mystery Person", phone="+15551234567")
    got = lookup_crm_context(phone="+15551234567", display_name=None)
    assert got is not None
    assert got.relationship is None
    assert got.company is None
    assert got.next_step is None


def test_bad_frontmatter_file_is_skipped(crm_dir):
    """A file with broken frontmatter doesn't crash the scan; lookup proceeds."""
    bad = crm_dir / "Broken.md"
    bad.write_text("---\nthis is: not: valid: yaml: at: all\n---\nbody", encoding="utf-8")
    write_crm_note(crm_dir, "Valid Person", phone="+15551234567")
    got = lookup_crm_context(phone="+15551234567", display_name=None)
    assert got is not None
    assert got.name == "Valid Person"


def test_next_step_field_returned(crm_dir):
    """The next_step frontmatter field is exposed in the returned context."""
    write_crm_note(
        crm_dir,
        "Strategic Person",
        phone="+15551234567",
        next_step="follow up Friday on contract",
    )
    got = lookup_crm_context(phone="+15551234567", display_name=None)
    assert got is not None
    assert got.next_step == "follow up Friday on contract"


def test_path_field_is_resolved_file(crm_dir):
    """The returned path field points at the actual matched file on disk."""
    written = write_crm_note(crm_dir, "Resolved Path", phone="+15551234567")
    got = lookup_crm_context(phone="+15551234567", display_name=None)
    assert got is not None
    assert Path(got.path).resolve() == written.resolve()
