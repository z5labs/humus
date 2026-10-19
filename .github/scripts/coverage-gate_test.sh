#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# Offline tests for coverage-gate.sh. Run from anywhere; exits non-zero when a
# case fails.

set -uo pipefail

gate="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/coverage-gate.sh"
dir=$(mktemp -d)
trap 'rm -r "$dir"' EXIT

failures=0

# expect NAME WANT-EXIT PACKAGES PROFILE
expect() {
  local name=$1 want=$2 packages=$3 profile=$4
  printf '%s' "$packages" >"$dir/packages.txt"
  printf '%s' "$profile" >"$dir/coverage.out"
  local out got
  out=$("$gate" 80 "$dir/packages.txt" "$dir/coverage.out" 2>&1)
  got=$?
  if [[ $got -ne $want ]]; then
    echo "FAIL $name: exit $got, want $want"
    echo "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

expect "a package at exactly 80% passes" 0 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 1\nexample.com/m/a/a.go:3.1,4.1 1 0\n'

expect "a package below 80% fails" 1 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 79 1\nexample.com/m/a/a.go:3.1,4.1 21 0\n'

expect "79.96% does not round up to 80%" 1 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1999 1\nexample.com/m/a/a.go:3.1,4.1 501 0\n'

expect "a package with no statements passes" 0 \
  $'example.com/m/doc\n' \
  $'mode: atomic\n'

expect "an empty package list gates nothing" 0 \
  '' \
  $'mode: atomic\nexample.com/m/example/x/x.go:1.1,2.1 9 0\n'

# The example module gates no package, so its list is empty; the gate must
# not read the profile's lines as packages to gate.
out=$("$gate" 80 "$dir/packages.txt" "$dir/coverage.out" 2>&1)
if [[ -n $out ]]; then
  echo "FAIL an empty package list reports no package"
  echo "$out" | sed 's/^/    /'
  failures=$((failures + 1))
else
  echo "ok   an empty package list reports no package"
fi

expect "a package with statements and no tests fails" 1 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 3 0\n'

expect "a block listed twice counts once, as covered if either covered it" 0 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 0\nexample.com/m/a/a.go:1.1,2.1 4 2\n'

expect "a package left out of the list is not gated" 0 \
  $'example.com/m/a\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nexample.com/m/example/x/x.go:1.1,2.1 9 0\n'

expect "a subpackage is gated apart from its parent" 1 \
  $'example.com/m/a\nexample.com/m/a/b\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nexample.com/m/a/b/b.go:1.1,2.1 1 0\n'

expect "profiles merged from two modules are each gated" 1 \
  $'example.com/m/a\nexample.com/m/n/c\n' \
  $'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 1 1\nmode: atomic\nexample.com/m/n/c/c.go:1.1,2.1 1 0\n'

usage() {
  local name=$1
  shift
  "$gate" "$@" >/dev/null 2>&1
  local got=$?
  if [[ $got -ne 2 ]]; then
    echo "FAIL $name: exit $got, want 2"
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

usage "a missing file is a usage error" 80 "$dir/missing.txt" "$dir/coverage.out"
usage "a non-numeric minimum is a usage error" 80% "$dir/packages.txt" "$dir/coverage.out"
usage "a minimum above 100 is a usage error" 101 "$dir/packages.txt" "$dir/coverage.out"
usage "too few arguments is a usage error" 80 "$dir/packages.txt"

if [[ $failures -ne 0 ]]; then
  echo "$failures case(s) failed"
  exit 1
fi
