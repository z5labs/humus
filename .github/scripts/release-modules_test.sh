#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# Offline tests for release-modules.sh, against throwaway repositories, each
# with a bare repository as its remote. They need git and go, and touch
# nothing outside a temporary directory. Run from anywhere; exits non-zero
# when a case fails.

set -uo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-modules.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com

failures=0
fail() {
  echo "FAIL $*"
  failures=$((failures + 1))
}

root=github.com/z5labs/humus
pseudo=v0.0.0-00010101000000-000000000000

# newrepo creates a repository whose remote origin is a bare repository, with
# one module per <dir>=<module path> argument (. for the root), commits it,
# pushes main and prints its directory. Each module holds one Go file, unless
# its argument starts with -, as the Hugo module under docs/ does not.
newrepo() {
  local dir
  dir=$(mktemp -d "$tmp/repo.XXXXXX")
  git init -q --bare "$dir.git"
  git init -q -b main "$dir"
  git -C "$dir" remote add origin "$dir.git"
  local arg mod
  for arg in "$@"; do
    mod=${arg#-}
    mkdir -p "$dir/${mod%%=*}"
    printf 'module %s\n\ngo 1.27.0\n' "${mod#*=}" >"$dir/${mod%%=*}/go.mod"
    if [[ $arg != -* ]]; then
      printf 'package p\n' >"$dir/${mod%%=*}/p.go"
    fi
  done
  commit "$dir" initial
  echo "$dir"
}

# commit commits every change in <repo> and pushes main.
commit() {
  git -C "$1" add -A
  git -C "$1" commit -q -m "$2"
  git -C "$1" push -q origin main
}

# run runs release-modules.sh in <repo> with the remaining arguments, leaving
# its combined output in $out and its exit status in $status.
run() {
  local repo=$1
  shift
  status=0
  out=$(cd "$repo" && "$script" "$@" 2>&1) || status=$?
}

remote_tags() {
  git -C "$1" ls-remote --tags origin | awk '{ print $2 }' | sort | tr '\n' ' '
}

expect_status() {
  if [[ $status != "$1" ]]; then
    fail "$2: exit $status, want $1"
    echo "$out" | sed 's/^/    /'
  fi
}

expect_output() {
  if ! grep -qF -- "$1" <<<"$out"; then
    fail "$2: output lacks \"$1\""
    echo "$out" | sed 's/^/    /'
  fi
}

reject_output() {
  if grep -qE -- "$1" <<<"$out"; then
    fail "$2: output matches /$1/"
    echo "$out" | sed 's/^/    /'
  fi
}

# Discovery finds every module, the root's included, and skips one under
# testdata, . or _, and one with no Go files of its own.
repo=$(newrepo .=$root grpc=$root/grpc queue/kafka=$root/queue/kafka -docs=$root \
  grpc/testdata/x=example.com/x _tools=example.com/tools queue/.hidden/m=example.com/m)
run "$repo" --dry-run v0.16.0
expect_status 0 "dry run"
expect_output "would create v0.16.0" "dry run"
expect_output "would create grpc/v0.16.0" "dry run"
expect_output "would create queue/kafka/v0.16.0" "dry run"
expect_output "skip docs/go.mod: it holds no Go packages" "dry run"
expect_output "skip grpc/testdata/x/go.mod: under testdata" "dry run"
expect_output "skip _tools/go.mod: under _tools" "dry run"
expect_output "skip queue/.hidden/m/go.mod: under .hidden" "dry run"
expect_output "dry run: changed nothing" "dry run"
reject_output 'would create (docs|grpc/testdata|_tools|queue/\.hidden)' "dry run"
[[ $(remote_tags "$repo") == "" ]] || fail "dry run: remote tags changed: $(remote_tags "$repo")"
[[ $(git -C "$repo" tag) == "" ]] || fail "dry run: local tags changed"

out=$(cd "$repo" && "$script" --list v0.16.0 2>/dev/null) || fail "list: exit $?"
[[ $out == "$root"$'\n'"$root/grpc"$'\n'"$root/queue/kafka" ]] || fail "list: got"$'\n'"$out"

# A Go file under testdata, . or _ is not a package, and a nested module's Go
# files are not its parent's.
repo=$(newrepo -.=$root -a=$root/a -b=$root/b -c=$root/c a/n=$root/a/n)
printf 'package p\n' >"$repo/p.go"
mkdir -p "$repo/b/testdata" "$repo/c/_x"
printf 'package p\n' >"$repo/b/testdata/p.go"
printf 'package p\n' >"$repo/c/_x/p.go"
commit "$repo" "Go files"
out=$(cd "$repo" && "$script" --list v0.16.0 2>&1) || fail "packages: exit $?"
[[ $out == *"skip a/go.mod: it holds no Go packages"* ]] || fail "packages: a has none; got"$'\n'"$out"
[[ $out == *"skip b/go.mod: it holds no Go packages"* ]] || fail "packages: b has none; got"$'\n'"$out"
[[ $out == *"skip c/go.mod: it holds no Go packages"* ]] || fail "packages: c has none; got"$'\n'"$out"
[[ $out == *"$root/a/n"* ]] || fail "packages: a/n has one; got"$'\n'"$out"

# The root module does not claim a nested module's Go files, however short
# its directory's name.
repo=$(newrepo -.=$root d=$root/d)
out=$(cd "$repo" && "$script" --list v0.16.0 2>&1) || fail "short name: exit $?"
[[ $out == "skip go.mod: it holds no Go packages"$'\n'"$root/d" ]] || fail "short name: got"$'\n'"$out"

# No module under example/ is released, whatever it requires, and saying so
# for each one; an example's Go files are still its own, not its parent's.
repo=$(newrepo -.=$root grpc=$root/grpc example=$root/example \
  example/shoppingcart=$root/example/shoppingcart example/billing=$root/example/billing)
printf '\nrequire github.com/z5labs/humus/grpc v0.15.0\n' >>"$repo/example/shoppingcart/go.mod"
commit "$repo" examples
run "$repo" --dry-run v0.16.0
expect_status 0 "examples"
expect_output "skip example/go.mod: under example/, whose modules are never released" "examples"
expect_output "skip example/shoppingcart/go.mod: under example/, whose modules are never released" "examples"
expect_output "skip example/billing/go.mod: under example/, whose modules are never released" "examples"
expect_output "skip go.mod: it holds no Go packages" "examples"
expect_output "would create grpc/v0.16.0" "examples"
grep -v '^skip example/' <<<"$out" | grep -q example && fail "examples: names an example outside its skip:"$'\n'"$out"
out=$(cd "$repo" && "$script" --list v0.16.0 2>/dev/null) || fail "examples list: exit $?"
[[ $out == "$root/grpc" ]] || fail "examples list: got"$'\n'"$out"
run "$repo" v0.16.0
expect_status 0 "examples tag"
[[ $(remote_tags "$repo") == "refs/tags/grpc/v0.16.0 " ]] || fail "examples tag: remote tags are $(remote_tags "$repo")"

# No module under tools/ is released, whatever it requires, and saying so for
# each one; a tool's Go files are still its own, not its parent's.
repo=$(newrepo -.=$root grpc=$root/grpc tools/flaky=$root/tools/flaky tools/other=$root/tools/other)
printf '\nrequire github.com/z5labs/humus/grpc v0.15.0\n' >>"$repo/tools/flaky/go.mod"
commit "$repo" tools
run "$repo" --dry-run v0.16.0
expect_status 0 "tools"
expect_output "skip tools/flaky/go.mod: under tools/, whose modules are never released" "tools"
expect_output "skip tools/other/go.mod: under tools/, whose modules are never released" "tools"
expect_output "skip go.mod: it holds no Go packages" "tools"
expect_output "would create grpc/v0.16.0" "tools"
grep -v '^skip tools/' <<<"$out" | grep -q tools && fail "tools: names a tool outside its skip:"$'\n'"$out"
out=$(cd "$repo" && "$script" --list v0.16.0 2>/dev/null) || fail "tools list: exit $?"
[[ $out == "$root/grpc" ]] || fail "tools list: got"$'\n'"$out"
run "$repo" v0.16.0
expect_status 0 "tools tag"
[[ $(remote_tags "$repo") == "refs/tags/grpc/v0.16.0 " ]] || fail "tools tag: remote tags are $(remote_tags "$repo")"

# Tagging pushes the root tag and every nested tag at the release commit.
repo=$(newrepo .=$root grpc=$root/grpc queue/kafka=$root/queue/kafka -docs=$root)
run "$repo" v0.16.0
expect_status 0 "tag"
expect_output "created v0.16.0" "tag"
expect_output "created grpc/v0.16.0" "tag"
expect_output "created queue/kafka/v0.16.0" "tag"
want="refs/tags/grpc/v0.16.0 refs/tags/queue/kafka/v0.16.0 refs/tags/v0.16.0 "
[[ $(remote_tags "$repo") == "$want" ]] || fail "tag: remote tags are $(remote_tags "$repo"), want $want"
release=$(git -C "$repo" rev-parse HEAD)
for tag in v0.16.0 grpc/v0.16.0 queue/kafka/v0.16.0; do
  got=$(git -C "$repo.git" rev-parse "$tag^{commit}")
  [[ $got == "$release" ]] || fail "tag: $tag at $got, want $release"
done

# A re-run against tags already at the release commit changes nothing.
run "$repo" v0.16.0
expect_status 0 "re-run"
expect_output "exists v0.16.0 at the release commit, skipping" "re-run"
expect_output "exists grpc/v0.16.0 at the release commit, skipping" "re-run"
expect_output "nothing to create" "re-run"
[[ $(remote_tags "$repo") == "$want" ]] || fail "re-run: remote tags changed: $(remote_tags "$repo")"

# --commit names the release commit, and a module added after it is not
# tagged with it.
mkdir -p "$repo/later"
printf 'module %s\n' "$root/later" >"$repo/later/go.mod"
printf 'package p\n' >"$repo/later/p.go"
commit "$repo" later
run "$repo" --dry-run --commit "$release" v0.16.0
expect_status 0 "later module"
reject_output later "later module"
run "$repo" --dry-run --commit nonexistent v0.16.0
expect_status 1 "no such commit"
expect_output "nonexistent does not name a commit" "no such commit"

# The release commit defaults to HEAD, so the release tag existing at another
# commit fails the run, and nothing is pushed.
run "$repo" v0.16.0
expect_status 1 "conflict"
expect_output "v0.16.0 already exists at $release, not at the release commit" "conflict"
[[ $(remote_tags "$repo") == "$want" ]] || fail "conflict: pushed $(remote_tags "$repo")"

# A nested tag at a different commit fails the run, and nothing is pushed.
repo=$(newrepo .=$root grpc=$root/grpc job=$root/job)
git -C "$repo" commit -q --allow-empty -m other
git -C "$repo" push -q origin HEAD:refs/tags/grpc/v0.16.0
git -C "$repo" reset -q --hard HEAD~1
run "$repo" v0.16.0
expect_status 1 "nested conflict"
expect_output "grpc/v0.16.0 already exists at" "nested conflict"
[[ $(remote_tags "$repo") == "refs/tags/grpc/v0.16.0 " ]] || fail "nested conflict: pushed $(remote_tags "$repo")"

# An annotated tag is compared by the commit it points to.
repo=$(newrepo .=$root grpc=$root/grpc)
git -C "$repo" tag -a -m annotated grpc/v0.16.0
git -C "$repo" push -q origin grpc/v0.16.0
run "$repo" v0.16.0
expect_status 0 "annotated"
expect_output "exists grpc/v0.16.0 at the release commit, skipping" "annotated"
expect_output "created v0.16.0" "annotated"

# A requirement between humus modules that names another version refuses the
# release, listing each such requirement; a dry run shows what --bump would
# set; --bump sets them, leaving every other requirement alone; and the
# release then goes ahead.
repo=$(newrepo .=$root grpc=$root/grpc queue/kafka=$root/queue/kafka job=$root/job)
cat >>"$repo/grpc/go.mod" <<EOF

require github.com/z5labs/humus $pseudo

replace github.com/z5labs/humus => ../
EOF
cat >>"$repo/queue/kafka/go.mod" <<EOF

require (
	example.com/other v1.2.3
	github.com/z5labs/humus $pseudo // indirect
)

replace github.com/z5labs/humus => ../..
EOF
cat >>"$repo/job/go.mod" <<EOF

require (
	github.com/z5labs/humus v0.16.0
	github.com/z5labs/humus/grpc v0.15.0
)
EOF
commit "$repo" requirements
before=$(git -C "$repo" rev-parse HEAD)

run "$repo" v0.16.0
expect_status 1 "stale"
expect_output "grpc/go.mod requires $root $pseudo, want v0.16.0" "stale"
expect_output "queue/kafka/go.mod requires $root $pseudo, want v0.16.0" "stale"
expect_output "job/go.mod requires $root/grpc v0.15.0, want v0.16.0" "stale"
expect_output "release-modules.sh --bump v0.16.0" "stale"
reject_output "job/go.mod requires $root v0.16.0" "stale"
reject_output "example.com/other" "stale"
[[ $(remote_tags "$repo") == "" ]] || fail "stale: pushed $(remote_tags "$repo")"

run "$repo" --dry-run v0.16.0
expect_status 0 "stale dry run"
expect_output "would set grpc/go.mod: require $root v0.16.0 (now $pseudo)" "stale dry run"
expect_output "would set job/go.mod: require $root/grpc v0.16.0 (now v0.15.0)" "stale dry run"
expect_output "release v0.16.0 at the commit that sets these requirements on top of $before" "stale dry run"
expect_output "would create grpc/v0.16.0" "stale dry run"
[[ $(git -C "$repo" status --porcelain) == "" ]] || fail "stale dry run: changed the working tree"

run "$repo" --bump v0.16.0
expect_status 0 "bump"
expect_output "set grpc/go.mod: require $root v0.16.0 (was $pseudo)" "bump"
expect_output "set queue/kafka/go.mod: require $root v0.16.0 (was $pseudo)" "bump"
expect_output "set job/go.mod: require $root/grpc v0.16.0 (was v0.15.0)" "bump"
grep -qF "github.com/z5labs/humus v0.16.0 // indirect" "$repo/queue/kafka/go.mod" ||
  fail "bump: lost the // indirect comment:"$'\n'"$(cat "$repo/queue/kafka/go.mod")"
grep -qF "example.com/other v1.2.3" "$repo/queue/kafka/go.mod" ||
  fail "bump: changed another requirement:"$'\n'"$(cat "$repo/queue/kafka/go.mod")"
grep -qF "replace github.com/z5labs/humus => ../.." "$repo/queue/kafka/go.mod" ||
  fail "bump: dropped the replace:"$'\n'"$(cat "$repo/queue/kafka/go.mod")"

run "$repo" --bump v0.16.0
expect_status 0 "bump again"
expect_output "every requirement between humus modules already names v0.16.0" "bump again"

commit "$repo" bump
run "$repo" v0.16.0
expect_status 0 "after bump"
expect_output "created v0.16.0" "after bump"
expect_output "created job/v0.16.0" "after bump"

# A dry run before the bump cannot know the commit the tags will go on, so any
# tag that already exists is a conflict.
repo=$(newrepo .=$root grpc=$root/grpc)
printf '\nrequire github.com/z5labs/humus %s\n' "$pseudo" >>"$repo/grpc/go.mod"
commit "$repo" requirement
git -C "$repo" push -q origin HEAD:refs/tags/grpc/v0.16.0
run "$repo" --dry-run v0.16.0
expect_status 1 "stale conflict"
expect_output "grpc/v0.16.0 already exists at" "stale conflict"

# Malformed versions, v2 and later, and mismatched module paths are refused
# before anything is created.
repo=$(newrepo .=$root grpc=$root/grpc)
for version in 0.16.0 v0.16 v0.16.0+build v01.2.3 v0.16.0- v0.16.0-rc..1 v0.16.0-rc. v0.16.0-01; do
  run "$repo" "$version"
  expect_status 1 "malformed $version"
  expect_output "$version is not a Go module version" "malformed $version"
done
run "$repo" v0.16.0+build
expect_output "build metadata" "build metadata"
for version in v0.16.0-rc.1 v1.0.0 v1.0.0-0.alpha-1; do
  run "$repo" --dry-run "$version"
  expect_status 0 "valid $version"
  expect_output "would create grpc/$version" "valid $version"
done
run "$repo" v2.0.0
expect_status 1 "v2"
expect_output "v2.0.0 has major version 2" "v2"
run "$repo" --bump v2.0.0
expect_status 1 "bump v2"
expect_output "v2.0.0 has major version 2" "bump v2"
[[ $(remote_tags "$repo") == "" ]] || fail "refused versions: pushed $(remote_tags "$repo")"

repo=$(newrepo .=$root grpc=$root/grpc wrong=$root/right)
run "$repo" v0.16.0
expect_status 1 "mismatch"
expect_output "wrong/go.mod declares module $root/right, want $root/wrong" "mismatch"
[[ $(remote_tags "$repo") == "" ]] || fail "mismatch: pushed $(remote_tags "$repo")"
run "$repo" --bump v0.16.0
expect_status 1 "bump mismatch"
expect_output "wrong/go.mod declares module $root/right, want $root/wrong" "bump mismatch"

# The script works from any directory of the repository.
repo=$(newrepo .=$root grpc=$root/grpc)
status=0
out=$(cd "$repo/grpc" && "$script" --dry-run v0.16.0 2>&1) || status=$?
expect_status 0 "subdirectory"
expect_output "would create grpc/v0.16.0" "subdirectory"

# Usage errors.
for args in "" "--dry-run --list v0.16.0" "--bump --dry-run v0.16.0" "--bump --commit HEAD v0.16.0" \
  "--bogus v0.16.0" "v0.16.0 v0.17.0" "--commit" "--remote"; do
  # shellcheck disable=SC2086 # the arguments are split on purpose
  run "$repo" $args
  expect_status 2 "usage: $args"
done

if [[ $failures -ne 0 ]]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "ok"
