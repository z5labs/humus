#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# cloud-setup.sh
#
# Everything a Claude Code cloud environment's setup script does for this
# repository, so the environment holds one line and the rest is versioned
# here:
#
#   bash /home/user/humus/.claude/hooks/cloud-setup.sh || true
#
# The setup script runs as root before Claude Code starts, which is the only
# point where all three of these take effect from a session's first prompt:
#
#   1. the `permissions` in .claude/settings.json, merged into user settings.
#      A cloud session drops a repository's `permissions.allow` rules, because
#      it never shows the workspace trust dialog, and keeps user settings,
#      which need no trust. Each list is unioned with what user settings
#      already hold, so nothing there is lost;
#   2. install-plugins.sh, which declares the plugins .claude/settings.json
#      enables in user settings, and registers and installs them before
#      Claude Code starts;
#   3. install-golangci-lint.sh, which installs golangci-lint at the version
#      .github/workflows/build.yaml pins.
#
# Both scripts also run as SessionStart hooks, on every session. A cloud
# environment keeps a snapshot of what its setup script wrote and starts later
# sessions from it without running the script again, until the script or the
# allowed hosts change, or the snapshot expires after about seven days
# (https://code.claude.com/docs/en/cloud-environments#environment-caching).
# The hooks keep the golangci-lint pin and the plugin settings current between
# those rebuilds.
#
# This is for a cloud environment's setup script, not a laptop: it writes the
# repository's permission list into user settings, which apply to every
# repository Claude Code opens.
#
# Usage: cloud-setup.sh [repo-root]   (default: the repository holding this file)
#
# Writes ${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json, merging into what is
# there. Always exits 0: a setup script that fails stops the session from
# starting, and a session without one of these is better than no session.

set -uo pipefail

say() { printf 'cloud-setup: %s\n' "$*"; }

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=${1:-$(cd "$here/../.." && pwd)}
[ -d "$root" ] || { say "no repository at '$root'; nothing set up"; exit 0; }
root=$(cd "$root" && pwd)

# --- 1. permissions ---------------------------------------------------------------

project="$root/.claude/settings.json"
dir=${CLAUDE_CONFIG_DIR:-$HOME/.claude}
settings="$dir/settings.json"

merge_permissions() {
  local perms current merged tmp
  command -v jq >/dev/null 2>&1 || { say "jq is not installed; permissions not copied"; return; }
  perms=$(jq -ec '.permissions | objects' "$project" 2>/dev/null) ||
    { say "$project has no permissions object; permissions not copied"; return; }
  current='{}'
  if [ -s "$settings" ]; then
    current=$(jq -ec 'objects' "$settings" 2>/dev/null) ||
      { say "$settings is not a JSON object; left as it is, permissions not copied"; return; }
  fi
  # Arrays are unioned in order, so an entry already there keeps its place;
  # anything else the repository sets replaces the user's value.
  merged=$(jq --argjson p "$perms" '
    .permissions = reduce ($p | to_entries[]) as $e ((.permissions // {});
      if ($e.value | type) == "array"
      then .[$e.key] = reduce $e.value[] as $v ((.[$e.key] // []); if index([$v]) then . else . + [$v] end)
      else .[$e.key] = $e.value end)
  ' <<<"$current") || { say "could not merge the permissions into $settings"; return; }
  [ "$(jq -S . <<<"$current")" = "$(jq -S . <<<"$merged")" ] && return
  mkdir -p "$dir" && tmp=$(mktemp "$dir/.settings.json.XXXXXX") ||
    { say "cannot write in $dir; permissions not copied"; return; }
  if printf '%s\n' "$merged" >"$tmp" && mv "$tmp" "$settings"; then
    say "copied the permissions in $project into $settings"
  else
    rm -f "$tmp"
    say "cannot write $settings; permissions not copied"
  fi
}
merge_permissions

# --- 2 and 3. the plugins and golangci-lint ------------------------------------------

# Each says what it did, and what it could not, itself.
bash "$here/install-plugins.sh" "$root" || say "install-plugins.sh exited $?"
bash "$here/install-golangci-lint.sh" "$root" || say "install-golangci-lint.sh exited $?"
exit 0
