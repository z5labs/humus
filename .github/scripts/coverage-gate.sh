#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# coverage-gate.sh MIN PACKAGES PROFILE
#
# Fails when any package listed in PACKAGES, one import path per line, has
# less than MIN percent statement coverage in PROFILE, a Go cover profile
# that may merge the profiles of several modules.
#
# A package with no statements passes. A package with statements but no
# tests is in the profile with every block unexecuted, so it counts as
# uncovered rather than being skipped. Statements are compared exactly, so
# 79.96% does not pass by rounding to 80.0%.
#
# Exit status: 0 when every package passes, 1 when one does not, and 2 on a
# usage error.

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 MIN PACKAGES PROFILE" >&2
  exit 2
fi

min=$1
packages=$2
profile=$3

if ! [[ $min =~ ^[0-9]+$ ]] || ((min > 100)); then
  echo "$0: MIN must be an integer from 0 to 100, got: $min" >&2
  exit 2
fi
for f in "$packages" "$profile"; do
  if [[ ! -r $f ]]; then
    echo "$0: cannot read $f" >&2
    exit 2
  fi
done

awk -v min="$min" '
  NR == FNR { if ($0 != "") gated[$0] = 1; next }
  /^mode:/ { next }
  {
    # A block can be listed more than once, by more than one test binary;
    # count it once, as covered if any listing covered it.
    stmts[$1] = $2
    if ($3 > 0) hit[$1] = 1
  }
  END {
    for (block in stmts) {
      pkg = block
      sub(/\/[^\/]*:.*$/, "", pkg)
      total[pkg] += stmts[block]
      if (block in hit) covered[pkg] += stmts[block]
    }
    failed = 0
    for (pkg in gated) {
      if (total[pkg] == 0) {
        printf "%s: no statements\n", pkg
        continue
      }
      pct = 100 * covered[pkg] / total[pkg]
      if (100 * covered[pkg] < min * total[pkg]) {
        printf "::error title=Coverage below %d%%::%s: %.1f%% of statements (%d of %d)\n", min, pkg, pct, covered[pkg], total[pkg]
        failed = 1
      } else {
        printf "%s: %.1f%% of statements (%d of %d)\n", pkg, pct, covered[pkg], total[pkg]
      }
    }
    exit failed
  }
' "$packages" "$profile"
