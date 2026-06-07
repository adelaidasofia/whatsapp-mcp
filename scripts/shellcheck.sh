#!/usr/bin/env bash
#
# scripts/shellcheck.sh - the canonical, locally-runnable shellcheck gate for
# whatsapp-mcp. ONE command, shared by two callers so they can never drift:
#
#   1. .github/workflows/shellcheck.yml - the `shellcheck` job runs
#                                    `bash scripts/shellcheck.sh`.
#   2. make shellcheck               - the local target runs the SAME script, so a
#                                    laptop run and CI cannot drift.
#
# Why this exists: whatsapp-mcp ships setup scripts (scripts/check_prerequisites.sh)
# that users run on macOS AND Linux when bringing up the bridge. A portability or
# quoting bug breaks a user's setup silently. `bash -n` is syntax-only; it cannot
# catch the static-analysis class: BSD-only flags, unquoted expansions (SC2086),
# unset vars, a `cd` whose failure runs the next command in the wrong directory, or
# an exit code masked by a pipe. The gate catches that whole class before it ships.
#
# Severity gate: -S warning (error + warning). info/style are NOT failed here, so
# the gate matches the real ship/hold boundary, not the strictest possible signal
# (an over-strict gate just teaches people to bypass it). Raise the floor later, on
# purpose, if the baseline supports it:  SHELLCHECK_SEVERITY=style bash scripts/shellcheck.sh
#
# A genuine false-positive is silenced at the source with an inline disable
# directive carrying a one-line reason - never by lowering the gate for every file.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)"
cd "$REPO_ROOT"

if ! command -v shellcheck >/dev/null 2>&1; then
  echo "::error::shellcheck not installed." >&2
  echo "  macOS:         brew install shellcheck" >&2
  echo "  Debian/Ubuntu: sudo apt-get install -y shellcheck" >&2
  exit 1
fi

SEVERITY="${SHELLCHECK_SEVERITY:-warning}"

# Collect every tracked *.sh. git ls-files is the source of truth: it excludes
# .git/, node_modules/, and untracked cruft for free, and -z is NUL-delimited so
# paths with spaces survive. Built bash-3.2-safe (macOS ships bash 3.2): no
# `mapfile -d`, just a read + append loop.
files=()
while IFS= read -r -d '' f; do
  files+=("$f")
done < <(git ls-files -z -- '*.sh')

# Empty-array expansion under `set -u` errors on bash 3.2 / 4.3; guard it.
if [ "${#files[@]}" -eq 0 ]; then
  echo "no tracked *.sh found - nothing to check"
  exit 0
fi

echo "==> shellcheck -S $SEVERITY over ${#files[@]} tracked *.sh  [$(shellcheck --version | awk '/^version:/{print $2}')]"

# GitHub Actions renders one annotation per finding from the gcc format; an
# interactive run gets shellcheck's readable default.
fmt="tty"
[ -n "${GITHUB_ACTIONS:-}" ] && fmt="gcc"

if shellcheck -S "$SEVERITY" -f "$fmt" "${files[@]}"; then
  echo "    OK - no shellcheck findings at -S $SEVERITY"
else
  echo "FAILED: shellcheck found issues at -S $SEVERITY." >&2
  echo "Fix them, or - for a genuine false-positive - add an inline shellcheck" >&2
  echo "disable directive with a one-line reason at the source line." >&2
  echo "Do NOT lower the severity gate to hide a finding (see this script's header)." >&2
  exit 1
fi
