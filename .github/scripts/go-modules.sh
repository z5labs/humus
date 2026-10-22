#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# go-modules.sh
#
# Prints, one per line and relative to the repository root, the directory of
# every Go module in the repository that holds at least one Go package. `./...`
# does not descend into a nested module, so CI builds, lints and tests each
# module listed here on its own.
#
# A module with no Go packages, such as the Hugo module under docs/, has
# nothing to build, lint or test; it is reported on stderr and left out.
#
# The flaky module under tools/ is listed like any other, so CI lints, builds
# and tests it.

set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"

# A go.mod under a testdata directory, or under a directory whose name begins
# with . or _, is a test's fixture rather than a module of the repository, as
# the Go tool ignores those directories too.
mapfile -t modules < <(git ls-files -- 'go.mod' '**/go.mod' |
  grep -Ev '(^|/)(testdata|[._][^/]*)/' | xargs -n1 dirname | sort -u)

for module in "${modules[@]}"; do
  # A failing go list is a broken module, not an empty one, so it fails
  # rather than being skipped.
  if ! packages=$(go -C "$module" list ./... 2>/dev/null); then
    echo "go-modules: go list failed in $module:" >&2
    go -C "$module" list ./... >/dev/null || true
    exit 1
  fi
  if [[ -n $packages ]]; then
    echo "$module"
  else
    echo "go-modules: $module holds no Go packages; skipping it" >&2
  fi
done
