#!/usr/bin/env bash
# check_prerequisites.sh: validate every dependency the MCP needs.
# Exits 0 if OK, non-zero with a clear message on the first missing prereq.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

ok() { echo -e "${GREEN}OK${NC} $1"; }
warn() { echo -e "${YELLOW}WARN${NC} $1"; }
fail() { echo -e "${RED}FAIL${NC} $1"; exit 1; }

# Go
if ! command -v go >/dev/null 2>&1; then
  fail "Go not found. Install via 'brew install go' (macOS) or your platform equivalent."
fi
GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
if [ "$GO_MAJOR" -lt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt 24 ]; }; then
  fail "Go $GO_VERSION too old. Need 1.24+."
fi
ok "Go $GO_VERSION"

# Python
if ! command -v python3 >/dev/null 2>&1; then
  fail "python3 not found."
fi
PY_VERSION=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
PY_MAJOR=$(echo "$PY_VERSION" | cut -d. -f1)
PY_MINOR=$(echo "$PY_VERSION" | cut -d. -f2)
if [ "$PY_MAJOR" -lt 3 ] || { [ "$PY_MAJOR" -eq 3 ] && [ "$PY_MINOR" -lt 11 ]; }; then
  fail "Python $PY_VERSION too old. Need 3.11+."
fi
ok "Python $PY_VERSION"

# uv
if ! command -v uv >/dev/null 2>&1; then
  fail "uv not found. Install: curl -LsSf https://astral.sh/uv/install.sh | sh"
fi
UV_VERSION=$(uv --version | awk '{print $2}')
ok "uv $UV_VERSION"

# FastMCP (via uv or system Python)
if ! python3 -c "import fastmcp" >/dev/null 2>&1; then
  warn "fastmcp not importable by system python3. It will be installed in the uv environment when the MCP runs; not fatal."
else
  FM_VERSION=$(python3 -c "import fastmcp; print(fastmcp.__version__)")
  ok "fastmcp $FM_VERSION (system)"
fi

# FFmpeg
if ! command -v ffmpeg >/dev/null 2>&1; then
  fail "ffmpeg not found. Install via 'brew install ffmpeg'. Required for voice-note audio conversion."
fi
FFMPEG_VERSION=$(ffmpeg -version 2>&1 | head -1 | awk '{print $3}')
ok "ffmpeg $FFMPEG_VERSION"

# SQLCipher
if ! command -v sqlcipher >/dev/null 2>&1; then
  fail "sqlcipher CLI not found. Install via 'brew install sqlcipher'. Required for encrypted database at rest."
fi
SC_VERSION=$(sqlcipher --version 2>&1 | head -1 | awk '{print $1}')
ok "sqlcipher $SC_VERSION"

# whisper.cpp (optional at setup, required at runtime if WHATSAPP_WHISPER_BACKEND=local-cpp)
if command -v whisper-cli >/dev/null 2>&1; then
  WC_VERSION=$(whisper-cli --help 2>&1 | head -1 | awk '{print $NF}' || echo "unknown")
  ok "whisper-cli ($WC_VERSION)"
elif command -v whisper-cpp >/dev/null 2>&1; then
  ok "whisper-cpp present"
else
  warn "whisper.cpp not found. Install via 'brew install whisper-cpp' if you plan to use the default local Whisper backend. Skippable if you use openai-api backend."
fi

# gh (optional, only for contributors)
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  ok "gh (GitHub CLI) authenticated"
fi

# macOS Keychain access (only relevant on Darwin)
if [ "$(uname -s)" = "Darwin" ]; then
  if command -v security >/dev/null 2>&1; then
    ok "macOS 'security' CLI present (Keychain access for SQLCipher key)"
  else
    fail "macOS 'security' CLI not found. Expected part of macOS base system."
  fi
fi

echo
ok "All prerequisites satisfied."
