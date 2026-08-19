#!/usr/bin/env bash
# check_prerequisites.sh: report what's present, what's required, and what's
# only needed for source builds. Unix twin of check_prerequisites.ps1.
#
# Exit code: 1 if a REQUIRED tool (python 3.11+, uv) is missing; 0 otherwise.
# Go and a C compiler are needed ONLY to build the bridge from source — the
# prebuilt release binary needs neither.
#
# This deliberately does NOT stop at the first problem. The previous version
# exited on the first missing tool, and because it also treated Go as
# required, anyone installing from a release binary got a red "FAIL Go not
# found" and exit 1 as their very first experience — and never saw the checks
# for the two things they actually needed.

set -uo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
DIM='\033[2m'
NC='\033[0m'

failures=0

ok()      { echo -e "  ${GREEN}OK${NC}       $1"; }
missing() { echo -e "  ${RED}MISSING${NC}  $1"; failures=$((failures + 1)); }
absent()  { echo -e "  ${DIM}absent${NC}   $1"; }
note()    { echo -e "  ${YELLOW}note${NC}     $1"; }

# Resolve a python interpreter: python3 everywhere, python as the fallback
# for MSYS/Git Bash where python3 often does not exist.
PYBIN=""
for cand in python3 python; do
  if command -v "$cand" >/dev/null 2>&1; then PYBIN="$cand"; break; fi
done

echo "whatsapp-mcp prerequisites ($(uname -s))"
echo
echo "Required for the MCP server:"

if [ -z "$PYBIN" ]; then
  missing "python    (required) -> install Python 3.11+"
else
  PY_VERSION=$("$PYBIN" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")' 2>/dev/null || echo "0.0")
  PY_MAJOR=${PY_VERSION%%.*}
  PY_MINOR=${PY_VERSION#*.}
  if [ "$PY_MAJOR" -lt 3 ] || { [ "$PY_MAJOR" -eq 3 ] && [ "$PY_MINOR" -lt 11 ]; }; then
    missing "python    $PY_VERSION is too old (need 3.11+)"
  else
    ok "python    $PY_VERSION"
  fi
fi

if command -v uv >/dev/null 2>&1; then
  ok "uv        $(uv --version 2>/dev/null | awk '{print $2}')"
else
  missing "uv        (required) -> curl -LsSf https://astral.sh/uv/install.sh | sh"
fi

echo
echo "Required only when building the bridge from source (prebuilt release binary needs neither):"

if command -v go >/dev/null 2>&1; then
  GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
  GO_MAJOR=${GO_VERSION%%.*}
  GO_REST=${GO_VERSION#*.}
  GO_MINOR=${GO_REST%%.*}
  if [ "$GO_MAJOR" -lt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt 24 ]; }; then
    note "go        $GO_VERSION is too old for a source build (need 1.24+)"
  else
    ok "go        $GO_VERSION"
  fi
else
  absent "go        (optional) -> https://go.dev/dl/ , or 'brew install go' on macOS"
fi

if command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1 || command -v clang >/dev/null 2>&1; then
  ok "C compiler present (needed by go-sqlcipher's cgo build)"
else
  absent "C compiler (optional) -> macOS: xcode-select --install ; Linux: install build-essential"
fi

echo
echo "Optional (voice-note transcription only; off by default):"

if command -v ffmpeg >/dev/null 2>&1; then
  ok "ffmpeg    $(ffmpeg -version 2>&1 | head -1 | awk '{print $3}')"
else
  absent "ffmpeg    (optional) -> only for WHATSAPP_WHISPER_BACKEND=local-cpp"
fi

if command -v whisper-cli >/dev/null 2>&1; then
  ok "whisper-cli present"
elif command -v whisper-cpp >/dev/null 2>&1; then
  ok "whisper-cpp present"
else
  absent "whisper   (optional) -> 'brew install whisper-cpp' when you enable local-cpp"
fi

if command -v sqlcipher >/dev/null 2>&1; then
  ok "sqlcipher $(sqlcipher --version 2>&1 | head -1 | awk '{print $1}') (CLI, for inspecting the DB by hand)"
else
  absent "sqlcipher (optional) -> the bridge bundles its own; the CLI is only for manual inspection"
fi

if [ "$(uname -s)" = "Darwin" ]; then
  echo
  echo "macOS:"
  if command -v security >/dev/null 2>&1; then
    ok "security  CLI present (Keychain access for the SQLCipher key)"
  else
    missing "security  CLI not found — expected as part of the macOS base system"
  fi
fi

echo
if [ "$failures" -gt 0 ]; then
  echo -e "${RED}$failures required tool(s) missing.${NC} Install them and re-run."
  exit 1
fi
echo -e "${GREEN}All required tools present.${NC}"
exit 0
