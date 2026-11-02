#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# install-golangci-lint.sh
#
# Puts golangci-lint, at the version CI pins, first on a Claude Code cloud
# session's PATH.
#
# The first verify command in .claude/backlog.json runs `golangci-lint run` in
# every module. A cloud session's image ships a golangci-lint of its own, and
# it can be too old to lint this repository at all: on 2026-11-01 the image's
# 2.5.0, built with go1.25, stopped before linting anything with "the Go
# language version (go1.25) used to build golangci-lint is lower than the
# targeted Go version (1.27.0)".
#
# The version is the `version` input of the golangci-lint-action step in
# .github/workflows/build.yaml. That is the one pin: CI's lint job installs that
# version, and this reads it from there, so a bump there reaches cloud sessions
# with no change here. Like the action, it downloads the release archive from
# github.com/golangci/golangci-lint/releases, checks it against the release's
# checksums file, and puts the binary in BIN_DIR, ~/.local/bin by default,
# which a cloud session's PATH puts ahead of the image's /usr/local/bin.
#
# When the golangci-lint first on PATH already reports the pinned version, it
# does nothing and says nothing.
#
# Usage:
#   install-golangci-lint.sh             the SessionStart hook: does nothing
#                                        unless CLAUDE_CODE_REMOTE is true; the
#                                        repository is CLAUDE_PROJECT_DIR
#   install-golangci-lint.sh <repo-root> a cloud environment's setup script,
#                                        which runs before CLAUDE_CODE_REMOTE
#                                        is set
#
# Environment, for tests and odd sandboxes:
#   BIN_DIR   where the binary goes (default ~/.local/bin)
#   GOLANGCI_LINT_RELEASES
#             the URL release archives are downloaded under (default
#             https://github.com/golangci/golangci-lint/releases/download)
#
# Always exits 0: a session without the pinned golangci-lint is better than no
# session, and the lint verify command then fails with a message that says why.

set -uo pipefail

say() { printf 'install-golangci-lint: %s\n' "$*"; }

if [ $# -ge 1 ]; then
  root=$1
  mode=setup
else
  [ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0
  root=${CLAUDE_PROJECT_DIR:-}
  mode=hook
fi

[ -n "$root" ] && [ -d "$root" ] || { say "no repository root (got '${root}'); golangci-lint not installed"; exit 0; }

workflow="$root/.github/workflows/build.yaml"
bin=${BIN_DIR:-$HOME/.local/bin}
releases=${GOLANGCI_LINT_RELEASES:-https://github.com/golangci/golangci-lint/releases/download}
allow="allow github.com and release-assets.githubusercontent.com under the environment's Network access → Allowed domains"

# --- the pin ----------------------------------------------------------------

# The `version` input of every step that uses golangci-lint-action, without
# quotes, a trailing comment or a leading v. A step ends at the next list item.
pins=$(awk '
  /^[[:space:]]*-[[:space:]]/ { step = 0 }
  /uses:[[:space:]]*["'\'']?golangci\/golangci-lint-action@/ { step = 1 }
  step && /^[[:space:]]*version:/ {
    v = $0
    sub(/^[[:space:]]*version:[[:space:]]*/, "", v)
    sub(/[[:space:]]+#.*$/, "", v)
    gsub(/["'\'']/, "", v)
    sub(/^v/, "", v)
    sub(/[[:space:]]+$/, "", v)
    print v
  }
' "$workflow" 2>/dev/null | sort -u)

case $(printf '%s' "$pins" | grep -c '') in
  0)
    say "cannot read the golangci-lint-action version from $workflow; golangci-lint not installed"
    exit 0
    ;;
  1) ;;
  *)
    say "$workflow pins more than one golangci-lint version ($(paste -sd, - <<<"$pins" | sed 's/,/, /g')); golangci-lint not installed"
    exit 0
    ;;
esac
want=$pins

if ! [[ $want =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  say "$workflow pins golangci-lint '$want', which is not a release version; golangci-lint not installed"
  exit 0
fi

# version_of <binary> — the version a golangci-lint reports, without a leading v,
# or nothing when it is not one.
version_of() {
  [ -n "$1" ] && [ -x "$1" ] || return 0
  "$1" --version 2>/dev/null | sed -n -E 's/^golangci-lint has version v?([^ ]+).*/\1/p' | head -1
}

[ "$(version_of "$(command -v golangci-lint 2>/dev/null)")" = "$want" ] && exit 0

# --- the binary ---------------------------------------------------------------

# install_release — download the pinned release for this machine, check it, and
# put its golangci-lint in BIN_DIR. Says why and returns 1 when it cannot.
install_release() {
  local os arch name tmp sum got
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case $(uname -m) in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) say "no golangci-lint release for $(uname -m); golangci-lint not installed"; return 1 ;;
  esac
  name="golangci-lint-$want-$os-$arch"

  tmp=$(mktemp -d) || { say "cannot make a temporary directory; golangci-lint not installed"; return 1; }
  # A RETURN trap would outlive this function, so every return below cleans up.
  if ! curl -fsSL --max-time 30 -o "$tmp/checksums.txt" \
    "$releases/v$want/golangci-lint-$want-checksums.txt" 2>/dev/null; then
    say "cannot download golangci-lint $want's checksums from $releases; if that host is blocked, $allow"
    rm -rf "$tmp"
    return 1
  fi
  if ! curl -fsSL --max-time 120 -o "$tmp/$name.tar.gz" "$releases/v$want/$name.tar.gz" 2>/dev/null; then
    say "cannot download $name.tar.gz from $releases; if that host is blocked, $allow"
    rm -rf "$tmp"
    return 1
  fi

  sum=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/checksums.txt")
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$name.tar.gz" | awk '{ print $1 }')
  else
    got=$(shasum -a 256 "$tmp/$name.tar.gz" 2>/dev/null | awk '{ print $1 }')
  fi
  if [ -z "$sum" ] || [ "$sum" != "$got" ]; then
    say "$name.tar.gz does not match its checksum in golangci-lint-$want-checksums.txt; golangci-lint not installed"
    rm -rf "$tmp"
    return 1
  fi

  if ! tar -xzf "$tmp/$name.tar.gz" -C "$tmp" "$name/golangci-lint" 2>/dev/null; then
    say "$name.tar.gz holds no $name/golangci-lint; golangci-lint not installed"
    rm -rf "$tmp"
    return 1
  fi
  # Copied next to its destination and renamed over it, so a golangci-lint
  # already there is replaced whole, even while something runs it.
  if ! mkdir -p "$bin" || ! cp "$tmp/$name/golangci-lint" "$bin/.golangci-lint.$$" ||
    ! chmod 0755 "$bin/.golangci-lint.$$" || ! mv -f "$bin/.golangci-lint.$$" "$bin/golangci-lint"; then
    rm -f "$bin/.golangci-lint.$$"
    say "cannot write golangci-lint to $bin; golangci-lint not installed"
    rm -rf "$tmp"
    return 1
  fi
  rm -rf "$tmp"

  if [ "$(version_of "$bin/golangci-lint")" != "$want" ]; then
    say "installed golangci-lint to $bin, but it does not report version $want"
    return 1
  fi
  say "installed golangci-lint $want to $bin"
}

if [ "$(version_of "$bin/golangci-lint")" != "$want" ]; then
  install_release || exit 0
fi

# --- PATH ---------------------------------------------------------------------

found=$(command -v golangci-lint 2>/dev/null)
[ "$found" = "$bin/golangci-lint" ] && exit 0

line="export PATH=\"$bin:\$PATH\""
if [ "$mode" = hook ] && [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  # CLAUDE_ENV_FILE is sourced before every command the session runs, so the
  # line puts BIN_DIR first from here on.
  grep -qxF "$line" "$CLAUDE_ENV_FILE" 2>/dev/null || printf '%s\n' "$line" >>"$CLAUDE_ENV_FILE"
  say "put $bin ahead of ${found:-the rest of PATH} for the rest of the session"
else
  say "$bin is not ahead of ${found:-the rest of PATH} on PATH, so golangci-lint $want is not the one that runs; run: $line"
fi
exit 0
