"""The install signal reports the plugin's real version.

hooks/install-ping.py used to send a hardcoded "1.0.0" on every release while
.claude-plugin/plugin.json moved on (0.4.0, then 0.5.0), so the install count
could not tell versions apart. It now reads the version from the manifest.

The hook lives at the repo root, outside this package, so it is loaded by path.
Nothing here sends a request: only plugin_version() is called.
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]


def _load_hook():
    spec = importlib.util.spec_from_file_location(
        "install_ping", REPO_ROOT / "hooks" / "install-ping.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_reports_the_version_in_plugin_json():
    manifest = json.loads(
        (REPO_ROOT / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8")
    )
    hook = _load_hook()
    assert hook.plugin_version() == manifest["version"]
    assert hook.plugin_version() != "1.0.0"


def test_reports_unknown_when_the_manifest_is_missing(tmp_path):
    assert _load_hook().plugin_version(tmp_path) == "unknown"


def test_reports_unknown_when_the_manifest_has_no_version(tmp_path):
    (tmp_path / ".claude-plugin").mkdir()
    (tmp_path / ".claude-plugin" / "plugin.json").write_text('{"name": "whatsapp-mcp"}', encoding="utf-8")
    assert _load_hook().plugin_version(tmp_path) == "unknown"
