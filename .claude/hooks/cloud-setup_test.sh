#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# Offline tests for cloud-setup.sh. Run from anywhere; exits non-zero when a
# case fails. Each case runs a copy of the script in a repository of its own,
# beside stand-ins for install-plugins.sh and install-golangci-lint.sh that
# record how they were called, and writes user settings under a directory of
# its own.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

failures=0
fail() {
  echo "FAIL $1"
  [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/    /'
  failures=$((failures + 1))
}
pass() { echo "ok   $1"; }
check() { if eval "$2"; then pass "$1"; else fail "$1" "${3:-}"; fi; }

# fixture <settings.json contents> [stub exit code] — a repository holding the
# script, two stand-ins exiting with that code, and that .claude/settings.json.
fixture() {
  local r code=${2:-0} s
  r=$(mktemp -d "$dir/repo.XXXXXX")
  mkdir -p "$r/.claude/hooks"
  cp "$here/cloud-setup.sh" "$r/.claude/hooks/"
  for s in install-plugins.sh install-golangci-lint.sh; do
    printf '#!/usr/bin/env bash\necho "%s $*" >>"%s/calls"\nexit %s\n' "$s" "$r" "$code" >"$r/.claude/hooks/$s"
  done
  printf '%s' "$1" >"$r/.claude/settings.json"
  printf '%s' "$r"
}

# run <repo> <config dir> [args...] — the script, as a setup script runs it.
run() {
  local r=$1 c=$2
  shift 2
  CLAUDE_CONFIG_DIR="$c" bash "$r/.claude/hooks/cloud-setup.sh" "$@" 2>&1
}

project='{"permissions":{"allow":["Read","Bash(go test*)"],"deny":["Bash(rm -rf*)"]},"hooks":{"SessionStart":[]}}'

# --- the permissions --------------------------------------------------------------

r=$(fixture "$project")
c="$dir/fresh"
out=$(run "$r" "$c" "$r")
code=$?
check "copies the permissions into new user settings" \
  '[ "$(jq -c . "$c/settings.json")" = "{\"permissions\":{\"allow\":[\"Read\",\"Bash(go test*)\"],\"deny\":[\"Bash(rm -rf*)\"]}}" ]' \
  "$(cat "$c/settings.json" 2>&1); $out"
check "copies nothing else from the repository's settings" '! jq -e "has(\"hooks\")" "$c/settings.json" >/dev/null'
check "says it copied them, and exits 0" \
  '[ "$code" -eq 0 ] && [[ $out == *"copied the permissions in $r/.claude/settings.json into $c/settings.json"* ]]' "exit $code; $out"

out=$(run "$r" "$c" "$r")
check "a second run changes nothing and says nothing about permissions" \
  '[[ $out != *permissions* ]] && [ "$(jq -c .permissions.allow "$c/settings.json")" = "[\"Read\",\"Bash(go test*)\"]" ]' "$out"

c="$dir/existing"
mkdir -p "$c"
printf '%s' '{"model":"x","enabledPlugins":{"a@b":true},"permissions":{"allow":["Bash(ls*)","Read"],"deny":["Bash(git stash*)"],"defaultMode":"default"}}' >"$c/settings.json"
out=$(run "$r" "$c" "$r")
check "keeps what user settings already hold, and unions each list in order" \
  '[ "$(jq -c . "$c/settings.json")" = "{\"model\":\"x\",\"enabledPlugins\":{\"a@b\":true},\"permissions\":{\"allow\":[\"Bash(ls*)\",\"Read\",\"Bash(go test*)\"],\"deny\":[\"Bash(git stash*)\",\"Bash(rm -rf*)\"],\"defaultMode\":\"default\"}}" ]' \
  "$(jq -c . "$c/settings.json"); $out"

c="$dir/invalid"
mkdir -p "$c"
printf 'not json' >"$c/settings.json"
out=$(run "$r" "$c" "$r")
check "leaves user settings it cannot read as they are" \
  '[ "$(cat "$c/settings.json")" = "not json" ] && [[ $out == *"is not a JSON object; left as it is"* ]]' "$out"

r2=$(fixture '{"hooks":{}}')
c="$dir/none"
out=$(run "$r2" "$c" "$r2")
check "copies nothing when the repository sets no permissions" \
  '[ ! -e "$c/settings.json" ] && [[ $out == *"has no permissions object"* ]]' "$out"

# The repository's own settings: every allow and deny entry arrives.
r3=$(fixture "$(cat "$repo/.claude/settings.json")")
c="$dir/real"
run "$r3" "$c" "$r3" >/dev/null
check "copies this repository's own allow and deny lists whole" \
  '[ "$(jq -c .permissions "$c/settings.json")" = "$(jq -c .permissions "$repo/.claude/settings.json")" ]'

# --- the two install scripts ----------------------------------------------------------

r6=$(fixture "$project")
run "$r6" "$dir/order" "$r6" >/dev/null
check "runs install-plugins.sh, then install-golangci-lint.sh, each with the repository root" \
  '[ "$(cat "$r6/calls")" = "$(printf "install-plugins.sh %s\ninstall-golangci-lint.sh %s" "$r6" "$r6")" ]' \
  "$(cat "$r6/calls")"

check "still runs both when it could not copy the permissions" '[ "$(grep -c "" "$r2/calls")" -eq 2 ]' "$(cat "$r2/calls")"

r4=$(fixture "$project")
out=$(run "$r4" "$dir/default" )
check "defaults the root to the repository holding the script" \
  '[ "$(head -1 "$r4/calls")" = "install-plugins.sh $r4" ]' "$(cat "$r4/calls"); $out"

r5=$(fixture "$project" 3)
out=$(run "$r5" "$dir/failing" "$r5")
code=$?
check "exits 0 when both install scripts fail, and says so" \
  '[ "$code" -eq 0 ] && [[ $out == *"install-plugins.sh exited 3"* ]] && [[ $out == *"install-golangci-lint.sh exited 3"* ]]' "exit $code; $out"

out=$(run "$r" "$dir/missing" "$dir/no-such-repo")
code=$?
check "exits 0 and says so when the repository is missing" \
  '[ "$code" -eq 0 ] && [[ $out == *"no repository at"* ]] && [ ! -e "$dir/missing/settings.json" ]' "exit $code; $out"

if [[ $failures -gt 0 ]]; then
  echo "$failures case(s) failed"
  exit 1
fi
