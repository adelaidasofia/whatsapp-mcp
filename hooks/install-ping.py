#!/usr/bin/env python3
"""One-time install signal to myceliumai.co.

Idempotent (one sentinel per plugin per machine), silent, fire-and-forget.
Sends ONLY plugin name + version. No PII, no IP storage post-dedup.

Opt out by setting environment variable MYCELIUM_NO_PING=1 before launching
Claude Code. See README "Telemetry" section for details.
"""
import json
import os
import sys
import urllib.request
from pathlib import Path

PLUGIN_NAME = "whatsapp-mcp"
PLUGIN_ROOT = Path(__file__).resolve().parent.parent


def plugin_version(root=PLUGIN_ROOT):
    """The installed plugin's version, read from its own manifest.

    A literal here sent "1.0.0" for every release while plugin.json moved on,
    so the install signal could not tell versions apart. "unknown" when the
    manifest cannot be read: an honest gap beats a wrong number.
    """
    try:
        manifest = json.loads((root / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8"))
        version = manifest.get("version")
        return version if isinstance(version, str) and version else "unknown"
    except Exception:
        return "unknown"


def main():
    # Opt-out via env var
    if os.environ.get("MYCELIUM_NO_PING"):
        return 0
    sentinel_dir = Path.home() / ".mycelium"
    sentinel = sentinel_dir / f"onboarded-{PLUGIN_NAME}"
    if sentinel.exists():
        return 0
    try:
        sentinel_dir.mkdir(exist_ok=True)
        sentinel.touch()
    except Exception:
        return 0
    try:
        data = json.dumps({"plugin": PLUGIN_NAME, "version": plugin_version()}).encode()
        req = urllib.request.Request(
            "https://myceliumai.co/api/install",
            data=data,
            headers={"Content-Type": "application/json"},
        )
        urllib.request.urlopen(req, timeout=3).read()
    except Exception:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
