#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# Offline tests for install-golangci-lint.sh. Run from anywhere; exits non-zero
# when a case fails. Releases are served from a directory through file:// URLs,
# and each holds a stand-in golangci-lint that only reports its version.

set -uo pipefail

# Each case sets what it needs; a Claude Code session running these sets them
# too, and would turn every setup-mode case into a hook-mode one.
unset CLAUDE_CODE_REMOTE CLAUDE_PROJECT_DIR CLAUDE_ENV_FILE

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/install-golangci-lint.sh"
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

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
esac

# The tools the script runs, and no golangci-lint but the ones a case adds.
base_path=/usr/bin:/bin

# fake_lint <file> <version> — a stand-in golangci-lint reporting <version>.
fake_lint() {
  mkdir -p "$(dirname "$1")"
  printf '#!/bin/sh\necho "golangci-lint has version %s built with go1.27.3 from fake on 2026-01-01T00:00:00Z"\n' "$2" >"$1"
  chmod +x "$1"
}

# release <version> — a release of <version> under $dir/releases, laid out as
# github.com/golangci/golangci-lint/releases/download lays one out.
release() {
  local v=$1 name="golangci-lint-$1-$os-$arch" out="$dir/releases/v$1" stage
  stage=$(mktemp -d "$dir/stage.XXXXXX")
  fake_lint "$stage/$name/golangci-lint" "$v"
  mkdir -p "$out"
  tar -czf "$out/$name.tar.gz" -C "$stage" "$name"
  (cd "$out" && sha256sum "$name.tar.gz" >"golangci-lint-$v-checksums.txt")
}

# root <build.yaml contents> — a repository root holding that workflow.
root() {
  local r
  r=$(mktemp -d "$dir/root.XXXXXX")
  mkdir -p "$r/.github/workflows"
  printf '%s' "$1" >"$r/.github/workflows/build.yaml"
  printf '%s' "$r"
}

workflow() {
  cat <<EOF
jobs:
  lint:
    steps:
        - uses: actions/setup-go@0000000000000000000000000000000000000000 # v7
          with:
            go-version: '1.27.3'

        - name: Install golangci-lint
          uses: golangci/golangci-lint-action@0000000000000000000000000000000000000000 # v9
          with:
            version: $1
            install-only: true

        - name: Lint
          run: golangci-lint run
EOF
}

# run_script <bin-dir> <path> [args...] — the script in setup mode unless no
# args are given, with releases served from $dir/releases.
run_script() {
  local bin=$1 path=$2
  shift 2
  env -i HOME="$dir/home" PATH="$path" BIN_DIR="$bin" \
    GOLANGCI_LINT_RELEASES="file://$dir/releases" \
    ${CLAUDE_CODE_REMOTE:+CLAUDE_CODE_REMOTE="$CLAUDE_CODE_REMOTE"} \
    ${CLAUDE_PROJECT_DIR:+CLAUDE_PROJECT_DIR="$CLAUDE_PROJECT_DIR"} \
    ${CLAUDE_ENV_FILE:+CLAUDE_ENV_FILE="$CLAUDE_ENV_FILE"} \
    bash "$script" "$@" 2>&1
}

# reports <bin-dir> — the version <bin-dir>/golangci-lint reports, if any.
reports() {
  [ -x "$1/golangci-lint" ] || return 0
  "$1/golangci-lint" --version | sed -n -E 's/^golangci-lint has version ([^ ]+).*/\1/p'
}

release 2.15.0
release 2.14.0

# --- setup mode -----------------------------------------------------------------

r=$(root "$(workflow v2.15.0)")
bin="$dir/a/bin"
out=$(run_script "$bin" "$bin:$base_path" "$r")
if [ "$(reports "$bin")" = 2.15.0 ] && [ "$out" = "install-golangci-lint: installed golangci-lint 2.15.0 to $bin" ]; then
  pass "installs the pinned version into BIN_DIR"
else
  fail "installs the pinned version into BIN_DIR" "reports '$(reports "$bin")'; output: $out"
fi

out=$(run_script "$bin" "$bin:$base_path" "$r")
if [ -z "$out" ]; then
  pass "does nothing once the pinned version is first on PATH"
else
  fail "does nothing once the pinned version is first on PATH" "$out"
fi

# The pinned version somewhere else on PATH is enough: nothing is downloaded.
other="$dir/b/other"
fake_lint "$other/golangci-lint" 2.15.0
bin="$dir/b/bin"
out=$(GOLANGCI_LINT_RELEASES=unused run_script "$bin" "$other:$base_path" "$r")
if [ -z "$out" ] && [ ! -e "$bin/golangci-lint" ]; then
  pass "does nothing when another golangci-lint on PATH is the pinned version"
else
  fail "does nothing when another golangci-lint on PATH is the pinned version" "$out"
fi

stale="$dir/c/stale"
fake_lint "$stale/golangci-lint" 2.5.0
bin="$dir/c/bin"
out=$(run_script "$bin" "$bin:$stale:$base_path" "$r")
if [ "$(env -i PATH="$bin:$stale:$base_path" golangci-lint --version | awk '{ print $4 }')" = 2.15.0 ]; then
  pass "replaces a stale golangci-lint behind BIN_DIR on PATH"
else
  fail "replaces a stale golangci-lint behind BIN_DIR on PATH" "$out"
fi

r2=$(root "$(workflow v2.14.0)")
out=$(run_script "$bin" "$bin:$stale:$base_path" "$r2")
if [ "$(reports "$bin")" = 2.14.0 ]; then
  pass "follows the pin when it changes"
else
  fail "follows the pin when it changes" "reports '$(reports "$bin")'; output: $out"
fi

bin="$dir/d/bin"
out=$(run_script "$bin" "$stale:$bin:$base_path" "$r")
if [ "$(reports "$bin")" = 2.15.0 ] && [[ $out == *"$bin is not ahead of $stale/golangci-lint on PATH"*"export PATH=\"$bin:\$PATH\""* ]]; then
  pass "says how to put BIN_DIR first when a stale golangci-lint is ahead of it"
else
  fail "says how to put BIN_DIR first when a stale golangci-lint is ahead of it" "$out"
fi

# --- the pin --------------------------------------------------------------------

for pin in '"v2.15.0"' "'2.15.0'" 'v2.15.0 # pinned' '2.15.0'; do
  bin=$(mktemp -d "$dir/pin.XXXXXX")
  run_script "$bin" "$bin:$base_path" "$(root "$(workflow "$pin")")" >/dev/null
  if [ "$(reports "$bin")" = 2.15.0 ]; then
    pass "reads the pin written as version: $pin"
  else
    fail "reads the pin written as version: $pin" "reports '$(reports "$bin")'"
  fi
done

bin="$dir/e/bin"
r3=$(root "$(printf 'jobs:\n  lint:\n    steps:\n      - uses: golangci/golangci-lint-action@v9\n        with:\n          install-only: true\n      - uses: actions/setup-go@v6\n        with:\n          version: 2.15.0\n')")
out=$(run_script "$bin" "$bin:$base_path" "$r3")
if [ ! -e "$bin/golangci-lint" ] && [[ $out == *"cannot read the golangci-lint-action version"* ]]; then
  pass "reads no other step's version, and installs nothing without a pin"
else
  fail "reads no other step's version, and installs nothing without a pin" "$out"
fi

r4=$(root "$(workflow v2.15.0; workflow v2.14.0)")
out=$(run_script "$bin" "$bin:$base_path" "$r4")
if [ ! -e "$bin/golangci-lint" ] && [[ $out == *"pins more than one golangci-lint version (2.14.0, 2.15.0)"* ]]; then
  pass "installs nothing when two steps pin different versions"
else
  fail "installs nothing when two steps pin different versions" "$out"
fi

r5=$(root "$(workflow latest)")
out=$(run_script "$bin" "$bin:$base_path" "$r5")
if [ ! -e "$bin/golangci-lint" ] && [[ $out == *"pins golangci-lint 'latest', which is not a release version"* ]]; then
  pass "installs nothing for a pin that is not a release version"
else
  fail "installs nothing for a pin that is not a release version" "$out"
fi

# The repository's own workflow, with a release at the version it pins.
want=$(grep -A6 'golangci/golangci-lint-action@' "$repo/.github/workflows/build.yaml" |
  sed -n -E 's/^[[:space:]]*version:[[:space:]]*v?([0-9.]+).*/\1/p' | head -1)
if [ -z "$want" ]; then
  fail "reads the pin in this repository's build.yaml" "no version input found next to golangci-lint-action"
else
  release "$want"
  bin="$dir/f/bin"
  out=$(run_script "$bin" "$bin:$base_path" "$repo")
  if [ "$(reports "$bin")" = "$want" ]; then
    pass "reads the pin in this repository's build.yaml ($want)"
  else
    fail "reads the pin in this repository's build.yaml ($want)" "$out"
  fi
fi

# --- failed downloads -----------------------------------------------------------

bin="$dir/g/bin"
out=$(run_script "$bin" "$bin:$base_path" "$(root "$(workflow v9.9.9)")")
code=$?
if [ "$code" -eq 0 ] && [ ! -e "$bin/golangci-lint" ] && [[ $out == *"cannot download golangci-lint 9.9.9's checksums"*"Allowed domains"* ]]; then
  pass "names the hosts to allow when the release cannot be downloaded, and exits 0"
else
  fail "names the hosts to allow when the release cannot be downloaded, and exits 0" "exit $code; $out"
fi

release 2.13.0
printf '%s  golangci-lint-2.13.0-%s-%s.tar.gz\n' "$(printf '0%.0s' $(seq 64))" "$os" "$arch" \
  >"$dir/releases/v2.13.0/golangci-lint-2.13.0-checksums.txt"
out=$(run_script "$bin" "$bin:$base_path" "$(root "$(workflow v2.13.0)")")
if [ ! -e "$bin/golangci-lint" ] && [[ $out == *"does not match its checksum"* ]]; then
  pass "installs nothing from an archive that does not match its checksum"
else
  fail "installs nothing from an archive that does not match its checksum" "$out"
fi

# --- hook mode ------------------------------------------------------------------

bin="$dir/h/bin"
out=$(CLAUDE_PROJECT_DIR="$r" run_script "$bin" "$bin:$base_path")
if [ -z "$out" ] && [ ! -e "$bin/golangci-lint" ]; then
  pass "the hook does nothing outside a cloud session"
else
  fail "the hook does nothing outside a cloud session" "$out"
fi

out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$r" run_script "$bin" "$bin:$base_path")
if [ "$(reports "$bin")" = 2.15.0 ]; then
  pass "the hook installs in a cloud session"
else
  fail "the hook installs in a cloud session" "$out"
fi

env_file="$dir/h/env"
: >"$env_file"
bin="$dir/i/bin"
for _ in 1 2; do
  out=$(CLAUDE_CODE_REMOTE=true CLAUDE_PROJECT_DIR="$r" CLAUDE_ENV_FILE="$env_file" \
    run_script "$bin" "$stale:$bin:$base_path")
done
if [ "$(cat "$env_file")" = "export PATH=\"$bin:\$PATH\"" ] &&
  [ "$(env -i PATH="$stale:$bin:$base_path" bash -c ". '$env_file'; golangci-lint --version" | awk '{ print $4 }')" = 2.15.0 ]; then
  pass "the hook puts BIN_DIR first through CLAUDE_ENV_FILE, once"
else
  fail "the hook puts BIN_DIR first through CLAUDE_ENV_FILE, once" "$(cat "$env_file"); $out"
fi

out=$(CLAUDE_CODE_REMOTE=true run_script "$bin" "$bin:$base_path")
if [[ $out == *"no repository root"* ]]; then
  pass "the hook says so when there is no repository root"
else
  fail "the hook says so when there is no repository root" "$out"
fi

if [[ $failures -gt 0 ]]; then
  echo "$failures case(s) failed"
  exit 1
fi
