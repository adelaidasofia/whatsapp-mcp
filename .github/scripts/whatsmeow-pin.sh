#!/usr/bin/env bash
# whatsmeow-pin.sh — parse the go.mau.fi/whatsmeow pin out of a go.mod.
#
# Enforcement helper for SECURITY.md item 6. Kept OUT of the workflow yaml so
# the parsing logic is unit-testable: whatsmeow-pin.test.sh exercises it with
# negative controls (unchanged pin MUST NOT flag; real move MUST flag).
#
# No `pipefail` on purpose: `grep` legitimately exits 1 when the module is
# absent, and every extractor here ends in awk/sed which exits 0. Under
# pipefail an absent module would abort the caller instead of yielding the
# empty string the classifier expects.
set -u

# Strip CR so a CRLF checkout parses identically to LF.
_normalize() { printf '%s\n' "$1" | tr -d '\r'; }

# The require-line version token: pseudo-version (v0.0.0-<ts>-<12hex>) or a
# plain semver tag (v0.10.2). Empty when the module is absent.
#
# Handles BOTH go.mod require forms:
#   require (
#   	go.mau.fi/whatsmeow v0.10.2      <- block form, module in column 1
#   )
#   require go.mau.fi/whatsmeow v0.10.2  <- single-line form, module in column 2
# The optional `require[[:space:]]+` prefix is what makes the second form work.
# Positional field extraction (`awk '{print $2}'`) CANNOT handle both — it
# returns the module path for the single-line form — so the version is captured
# relative to the module path instead.
#
# The `[[:space:]]+` immediately after the module path is load-bearing: it stops
# `go.mau.fi/whatsmeow/proto v1.0.0` (a DIFFERENT module) from being read as the
# whatsmeow pin. Anchoring at `^[[:space:]]*` stops a `// go.mau.fi/whatsmeow`
# comment from matching at all.
whatsmeow_version() {
  _normalize "$1" \
    | sed -nE 's|^[[:space:]]*(require[[:space:]]+)?go\.mau\.fi/whatsmeow[[:space:]]+(v[^[:space:]]+).*|\2|p' \
    | head -n1
}

# Any replace directive whose LEFT side is the whatsmeow module, normalized for
# comparison.
#
# Why this exists: a replace can redirect the module to arbitrary code while the
# require line stays BYTE-IDENTICAL. Comparing require lines alone would report
# "unchanged" for
#     replace go.mau.fi/whatsmeow => github.com/attacker/evil v1.0.0
# which is a total bypass of the review gate. Matches both the standalone form
# and the indented form inside a `replace ( ... )` block.
whatsmeow_replace() {
  _normalize "$1" \
    | grep -E '^[[:space:]]*(replace[[:space:]]+)?go\.mau\.fi/whatsmeow([[:space:]]+v[^[:space:]]+)?[[:space:]]*=>' \
    | head -n1 \
    | sed -E 's/^[[:space:]]*(replace[[:space:]]+)?//; s/[[:space:]]+/ /g; s/[[:space:]]*$//'
}

# Upstream commit SHA from a pseudo-version. Empty for a plain semver tag —
# that is expected, not an error, and the caller degrades to "no commit range".
#
# NOTE: the `--` before the pattern is mandatory. The original implementation
# used `grep -oE '-[0-9a-f]{12}...'`; because that pattern BEGINS WITH A DASH,
# grep parsed it as an option bundle instead of a pattern and returned empty for
# EVERY input. That is the bug that made this gate flag every go.mod PR as
# indeterminate. Using sed here sidesteps the class entirely.
whatsmeow_sha() {
  printf '%s' "${1:-}" | sed -nE 's/^.*-([0-9a-f]{12})$/\1/p'
}

# classify_pin <old-go.mod-contents> <new-go.mod-contents>
#   -> yes | no | indeterminate
#
# indeterminate is the FAIL-SAFE: unparseable on either side means we cannot
# prove the pin held, so the caller escalates to human review.
classify_pin() {
  local old_mod="$1" new_mod="$2"
  local old_ver new_ver old_rep new_rep

  old_ver=$(whatsmeow_version "$old_mod")
  new_ver=$(whatsmeow_version "$new_mod")
  old_rep=$(whatsmeow_replace "$old_mod")
  new_rep=$(whatsmeow_replace "$new_mod")

  # Cannot prove anything about a pin we could not read.
  if [ -z "$old_ver" ] || [ -z "$new_ver" ]; then
    echo "indeterminate"
    return
  fi

  # A replace that appears, disappears, or retargets is a pin move even when
  # the require line is untouched.
  if [ "$old_rep" != "$new_rep" ]; then
    echo "yes"
    return
  fi

  # An UNCHANGED replace is not re-flagged: it was reviewed when introduced,
  # and re-flagging it on every subsequent PR rebuilds the alarm fatigue this
  # gate exists to avoid.
  if [ "$old_ver" = "$new_ver" ]; then
    echo "no"
  else
    echo "yes"
  fi
}

# CLI mode: whatsmeow-pin.sh <old-go.mod-path> <new-go.mod-path>
# Emits GitHub Actions output lines. Sourced (not executed) by the test file.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  [ $# -eq 2 ] || { echo "usage: $0 <old-go.mod> <new-go.mod>" >&2; exit 2; }
  OLD=$(cat "$1" 2>/dev/null || echo "")
  NEW=$(cat "$2" 2>/dev/null || echo "")
  CHANGED=$(classify_pin "$OLD" "$NEW")
  OLD_VER=$(whatsmeow_version "$OLD")
  NEW_VER=$(whatsmeow_version "$NEW")
  echo "changed=$CHANGED"
  echo "old_version=$OLD_VER"
  echo "new_version=$NEW_VER"
  echo "old_sha=$(whatsmeow_sha "$OLD_VER")"
  echo "new_sha=$(whatsmeow_sha "$NEW_VER")"
  echo "old_replace=$(whatsmeow_replace "$OLD")"
  echo "new_replace=$(whatsmeow_replace "$NEW")"
fi
