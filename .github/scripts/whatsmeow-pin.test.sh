#!/usr/bin/env bash
# Negative controls for the whatsmeow review gate.
#
# The gate previously failed OPEN TO NOISE: extract_sha returned empty for every
# input, so every go.mod PR tripped the indeterminate fail-safe and got the
# review-required label. A gate that fires on everything trains reviewers to
# ignore it, so the day the pin DOES move maliciously the label is
# indistinguishable from the 20 false ones before it.
#
# A gate earns trust only by failing on the thing it catches AND staying quiet
# on the thing it does not. Both directions are asserted here.
set -u

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "$DIR/whatsmeow-pin.sh"

PASS=0; FAIL=0

expect() { # expect <name> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  PASS  %-56s %s\n' "$1" "$2"; PASS=$((PASS+1))
  else
    printf '  FAIL  %-56s expected=%s actual=%s\n' "$1" "$2" "$3"; FAIL=$((FAIL+1))
  fi
}

PSEUDO_A='module whatsapp-bridge

go 1.25.0

require (
	go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7
	golang.org/x/crypto v0.52.0 // indirect
)'

# Same pin, unrelated dependency moved — the exact shape of PR #38.
PSEUDO_A_OTHER_DEP_MOVED='module whatsapp-bridge

go 1.25.0

require (
	go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7
	golang.org/x/crypto v0.53.0 // indirect
)'

PSEUDO_B='module whatsapp-bridge

go 1.25.0

require (
	go.mau.fi/whatsmeow v0.0.0-20260501120000-aaaaaaaaaaaa
	golang.org/x/crypto v0.52.0 // indirect
)'

TAG_A='module whatsapp-bridge
require go.mau.fi/whatsmeow v0.10.2'
TAG_B='module whatsapp-bridge
require go.mau.fi/whatsmeow v0.10.3'

REPLACE_ADDED='module whatsapp-bridge

require (
	go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7
)

replace go.mau.fi/whatsmeow => github.com/attacker/evil v1.0.0'

REPLACE_IN_BLOCK='module whatsapp-bridge

require (
	go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7
)

replace (
	go.mau.fi/whatsmeow => github.com/attacker/evil v1.0.0
)'

NO_MODULE='module whatsapp-bridge

require (
	golang.org/x/crypto v0.52.0 // indirect
)'

SUBMODULE_ONLY='module whatsapp-bridge

require (
	go.mau.fi/whatsmeow/proto v1.0.0
)'

COMMENTED='module whatsapp-bridge

// go.mau.fi/whatsmeow v9.9.9
require (
	golang.org/x/crypto v0.52.0 // indirect
)'

CRLF_A=$(printf 'module whatsapp-bridge\r\nrequire go.mau.fi/whatsmeow v0.0.0-20260421083005-5b8886176ff7\r\n')

echo "== NEGATIVE CONTROLS: gate must stay QUIET =="
expect "unchanged pseudo-version pin"              "no" "$(classify_pin "$PSEUDO_A" "$PSEUDO_A")"
expect "unchanged pin, other dep moved (PR #38)"   "no" "$(classify_pin "$PSEUDO_A" "$PSEUDO_A_OTHER_DEP_MOVED")"
expect "unchanged semver-tag pin"                  "no" "$(classify_pin "$TAG_A" "$TAG_A")"
expect "unchanged pin across CRLF/LF checkout"     "no" "$(classify_pin "$CRLF_A" "$(printf '%s' "$CRLF_A" | tr -d '\r')")"
expect "pre-existing replace, otherwise unchanged" "no" "$(classify_pin "$REPLACE_ADDED" "$REPLACE_ADDED")"

echo "== POSITIVE CONTROLS: gate must FIRE =="
expect "pseudo-version pin moved"                  "yes" "$(classify_pin "$PSEUDO_A" "$PSEUDO_B")"
expect "semver tag moved"                          "yes" "$(classify_pin "$TAG_A" "$TAG_B")"
expect "pseudo-version -> tag"                     "yes" "$(classify_pin "$PSEUDO_A" "$TAG_A")"
expect "replace directive ADDED (bypass attempt)"  "yes" "$(classify_pin "$PSEUDO_A" "$REPLACE_ADDED")"
expect "replace inside replace() block"            "yes" "$(classify_pin "$PSEUDO_A" "$REPLACE_IN_BLOCK")"
expect "replace directive REMOVED"                 "yes" "$(classify_pin "$REPLACE_ADDED" "$PSEUDO_A")"

echo "== FAIL-SAFE: unparseable must escalate =="
expect "module absent on new side"                 "indeterminate" "$(classify_pin "$PSEUDO_A" "$NO_MODULE")"
expect "module absent on old side"                 "indeterminate" "$(classify_pin "$NO_MODULE" "$PSEUDO_A")"
expect "empty base (new file)"                     "indeterminate" "$(classify_pin "" "$PSEUDO_A")"
expect "only a submodule present"                  "indeterminate" "$(classify_pin "$PSEUDO_A" "$SUBMODULE_ONLY")"
expect "only a commented-out mention"              "indeterminate" "$(classify_pin "$PSEUDO_A" "$COMMENTED")"

echo "== EXTRACTORS =="
expect "version from pseudo-version"  "v0.0.0-20260421083005-5b8886176ff7" "$(whatsmeow_version "$PSEUDO_A")"
expect "version from semver tag"      "v0.10.2"                           "$(whatsmeow_version "$TAG_A")"
expect "version ignores submodule"    ""                                  "$(whatsmeow_version "$SUBMODULE_ONLY")"
expect "version ignores comment"      ""                                  "$(whatsmeow_version "$COMMENTED")"
# Regression guard for the original defect: a leading-dash pattern made grep
# treat the regex as an option bundle and return empty for every input.
expect "sha from pseudo-version"      "5b8886176ff7"                      "$(whatsmeow_sha "v0.0.0-20260421083005-5b8886176ff7")"
expect "sha empty for semver tag"     ""                                  "$(whatsmeow_sha "v0.10.2")"
expect "sha empty for empty input"    ""                                  "$(whatsmeow_sha "")"

echo ""
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
