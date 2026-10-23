#!/usr/bin/env bash
# Copyright (c) 2026 Z5Labs and Contributors
#
# This software is released under the MIT License.
# https://opensource.org/licenses/MIT

# release-modules.sh
#
# Releases every Go module in this repository at one version. A nested
# module's versions are tags prefixed with its directory: grpc/v0.16.0 is
# version v0.16.0 of github.com/z5labs/humus/grpc. Every module is tagged with
# the same version on every release, whether or not it changed, after
# OpenTelemetry Go: one version identifies a consistent set of modules.
#
# The nested modules require the root module, and may require each other. A
# replace directive points each such requirement at the local directory, but a
# replace applies only in the main module, so an end user's build sees the
# required version itself. At every release, each requirement between humus
# modules therefore names the release's version. --bump sets them; tagging
# refuses a commit at which any of them names another version.
#
# The modules under example/ and tools/ are never tagged, but --bump sets
# their requirements on the released modules too, and tagging checks them
# with the rest. Every replace in such a module points at the local
# directories, so its build selects the highest version that it or any humus
# module it requires names. Once a nested module requires the root module at
# the release's version, an example still naming the last one has a go.mod
# that is out of date, and does not build.
#
# Usage:
#
#   release-modules.sh [--bump | --dry-run | --list] [--commit <rev>] [--remote <name>] <version>
#
#   <version>        vMAJOR.MINOR.PATCH with an optional pre-release suffix,
#                    e.g. v0.16.0 or v0.16.0-rc.1
#   (no mode)        check the release commit and create a tag for every
#                    module at it: <version> for the root module and
#                    <dir>/<version> for each nested module
#   --bump           set every requirement one humus module has on another to
#                    <version>, in the go.mod files of the working tree,
#                    those of the modules under example/ and tools/ included
#   --dry-run        make every check, print the requirements --bump would set,
#                    in the examples' and tools' go.mod files too, and the
#                    tags that would be created, and change nothing
#   --list           print the path of every module the release tags, one
#                    per line, and touch no remote
#   --commit <rev>   the release commit; defaults to HEAD. --bump works on the
#                    working tree, so it takes no --commit
#   --remote <name>  the remote tags are read from and pushed to; defaults to
#                    origin
#
# Before any tag is created, the script checks that <version> is a Go module
# version of major version 0 or 1, that every nested module's path is its
# directory under the root module's path, that every requirement between
# humus modules, and every requirement an example or a tool has on a released
# module, names <version>, and that no tag already exists at a different
# commit. A tag that already exists at the release commit is skipped, so a
# re-run is safe. The tags it does create are pushed in one atomic push, so a
# failure leaves no partial set.
#
# The modules are discovered at the release commit, not from a list kept by
# hand: every tracked go.mod except any the Go tool ignores, one under a
# testdata directory or under a directory whose name begins with . or _. A
# module that holds no Go files of its own, such as the Hugo module under
# docs/, has nothing to release and is skipped. So is every module under
# example/: an example is an application to copy, not a library to require.
# And so is every module under tools/: a tool is for the repository's
# maintainers, not a library to require. Neither is tagged or listed, but
# both have their requirements on the released modules set, as above.

set -euo pipefail

usage() {
  echo "usage: release-modules.sh [--bump | --dry-run | --list] [--commit <rev>] [--remote <name>] <version>" >&2
  exit 2
}

die() {
  echo "release-modules: $*" >&2
  exit 1
}

mode=tag
commit=
remote=origin
version=

while [[ $# -gt 0 ]]; do
  case "$1" in
  --bump | --dry-run | --list)
    [[ $mode == tag ]] || usage
    mode=${1#--}
    ;;
  --commit)
    [[ $# -ge 2 ]] || usage
    commit=$2
    shift
    ;;
  --remote)
    [[ $# -ge 2 ]] || usage
    remote=$2
    shift
    ;;
  -*)
    usage
    ;;
  *)
    [[ -z $version ]] || usage
    version=$1
    ;;
  esac
  shift
done
[[ -n $version ]] || usage
[[ $mode != bump || -z $commit ]] || usage

# The version must be one the Go toolchain accepts as a module version: a
# canonical semantic version with a leading v, an optional pre-release and no
# build metadata.
case "$version" in
*+*) die "$version is not a Go module version: build metadata (+...) is not allowed" ;;
esac
numeric='(0|[1-9][0-9]*)'
if ! [[ $version =~ ^v${numeric}\.${numeric}\.${numeric}(-([0-9A-Za-z.-]+))?$ ]]; then
  die "$version is not a Go module version: want vMAJOR.MINOR.PATCH with an optional -pre-release suffix"
fi
major=${BASH_REMATCH[1]}
prerelease=${BASH_REMATCH[5]}
if [[ -n $prerelease ]]; then
  # "a." splits to a single identifier, so a trailing dot is checked first.
  case "$prerelease" in
  *.) die "$version is not a Go module version: its pre-release has an empty identifier" ;;
  esac
  IFS=. read -r -a identifiers <<<"$prerelease"
  for identifier in "${identifiers[@]}"; do
    if [[ -z $identifier ]]; then
      die "$version is not a Go module version: its pre-release has an empty identifier"
    fi
    if [[ $identifier =~ ^0[0-9]+$ ]]; then
      die "$version is not a Go module version: numeric pre-release identifier $identifier has a leading zero"
    fi
  done
fi
if [[ $major -gt 1 ]]; then
  die "$version has major version $major: from v2 on every module path needs a /v$major suffix, which this repository does not have"
fi

# --bump reads and edits the working tree; every other mode reads the release
# commit, so a module added after it is not released with it.
cd "$(git rev-parse --show-toplevel)"
if [[ $mode == bump ]]; then
  files() { git ls-files; }
  show() { cat -- "$1"; }
else
  sha=$(git rev-parse --verify --quiet "${commit:-HEAD}^{commit}") || die "${commit:-HEAD} does not name a commit"
  files() { git ls-tree -r --full-tree --name-only "$sha"; }
  show() { git show "$sha:$1"; }
fi

# module_path prints the module path declared by the go.mod at <file>.
module_path() {
  show "$1" | awk '
    $1 == "module" {
      path = $2
      gsub(/"/, "", path)
      print path
      exit
    }
  '
}

# requirements prints the path and version of every requirement in the go.mod
# at <file>, one per line, in either the single-line or the block form.
requirements() {
  show "$1" | awk '
    $1 == "require" && $2 == "(" { block = 1; next }
    block && $1 == ")" { block = 0; next }
    block && $1 !~ /^\/\// && NF >= 2 { path = $1; v = $2 }
    !block && $1 == "require" && NF >= 3 { path = $2; v = $3 }
    path != "" {
      gsub(/"/, "", path)
      print path, v
      path = ""
    }
  '
}

gomod() {
  if [[ $1 == . ]]; then
    echo go.mod
  else
    echo "$1/go.mod"
  fi
}

mapfile -t tracked < <(files)

# Every go.mod the Go tool does not ignore is a module, the root's first.
dirs=()
for file in "${tracked[@]}"; do
  case "$file" in
  go.mod) dir=. ;;
  */go.mod) dir=${file%/go.mod} ;;
  *) continue ;;
  esac
  ignored=
  if [[ $dir != . ]]; then
    IFS=/ read -r -a parts <<<"$dir"
    for part in "${parts[@]}"; do
      case "$part" in
      testdata | .* | _*)
        ignored=$part
        break
        ;;
      esac
    done
  fi
  if [[ -n $ignored ]]; then
    echo "skip $file: under $ignored, which the Go tool ignores" >&2
    continue
  fi
  if [[ $dir == . ]]; then
    dirs=(. "${dirs[@]}")
  else
    dirs+=("$dir")
  fi
done
[[ ${#dirs[@]} -gt 0 && ${dirs[0]} == . ]] || die "there is no go.mod at the root of the repository"

# A Go file belongs to the module in the deepest directory that holds it, and
# is no package at all under a directory the Go tool ignores.
mapfile -t owners < <(
  awk '
    NR == FNR { mods[++n] = $0; next }
    /\.go$/ {
      k = split($0, parts, "/")
      for (i = 1; i <= k; i++) {
        if (parts[i] == "testdata" && i < k) next
        if (parts[i] ~ /^[._]/) next
      }
      owner = ""
      depth = -1
      for (j = 1; j <= n; j++) {
        d = mods[j]
        if (d == "." && depth < 0) {
          owner = d
          depth = 0
        }
        if (d != "." && index($0, d "/") == 1 && length(d) > depth) {
          owner = d
          depth = length(d)
        }
      }
      if (owner != "" && !(owner in seen)) {
        seen[owner] = 1
        print owner
      }
    }
  ' <(printf '%s\n' "${dirs[@]}") <(printf '%s\n' "${tracked[@]}")
)
declare -A has_packages=()
for dir in "${owners[@]}"; do
  has_packages[$dir]=1
done

root=$(module_path go.mod)
[[ -n $root ]] || die "go.mod declares no module path"

# An example's or a tool's module stays among dirs above, so its Go files are
# its own and not its parent's, but it is never released. Its requirements on
# the released modules are still pinned to the version, with theirs.
modules=()
pinned=()
for dir in "${dirs[@]}"; do
  case "$dir" in
  example | example/*)
    echo "skip $(gomod "$dir"): under example/, whose modules are never released" >&2
    pinned+=("$dir")
    continue
    ;;
  tools | tools/*)
    echo "skip $(gomod "$dir"): under tools/, whose modules are never released" >&2
    pinned+=("$dir")
    continue
    ;;
  esac
  if [[ -z ${has_packages[$dir]:-} ]]; then
    echo "skip $(gomod "$dir"): it holds no Go packages" >&2
    continue
  fi
  modules+=("$dir")
  pinned+=("$dir")
done

# Every nested module's tag resolves only if its path is its directory under
# the root module's path. Every mismatch is reported before anything happens.
declare -A released=()
mismatched=0
for dir in "${modules[@]}"; do
  if [[ $dir == . ]]; then
    released[$root]=1
    continue
  fi
  path=$(module_path "$dir/go.mod")
  if [[ $path != "$root/$dir" ]]; then
    echo "release-modules: $dir/go.mod declares module ${path:-<none>}, want $root/$dir: its tag $dir/$version would never resolve" >&2
    mismatched=1
  fi
  released[$root/$dir]=1
done
[[ $mismatched == 0 ]] || exit 1

if [[ $mode == list ]]; then
  for dir in "${modules[@]}"; do
    if [[ $dir == . ]]; then
      echo "$root"
    else
      echo "$root/$dir"
    fi
  done
  exit 0
fi

# Every requirement a released module, an example or a tool has on a released
# module that does not name the release's version.
stale=()
for dir in "${pinned[@]}"; do
  file=$(gomod "$dir")
  while read -r path have; do
    if [[ -n ${released[$path]:-} && $have != "$version" ]]; then
      stale+=("$file $path $have")
    fi
  done < <(requirements "$file")
done

if [[ $mode == bump ]]; then
  if [[ ${#stale[@]} -eq 0 ]]; then
    echo "every requirement between humus modules already names $version"
    exit 0
  fi
  for entry in "${stale[@]}"; do
    read -r file path have <<<"$entry"
    go mod edit -require="$path@$version" "$file"
    echo "set $file: require $path $version (was $have)"
  done
  exit 0
fi

if [[ ${#stale[@]} -gt 0 ]]; then
  if [[ $mode == tag ]]; then
    for entry in "${stale[@]}"; do
      read -r file path have <<<"$entry"
      echo "release-modules: $file requires $path $have, want $version" >&2
    done
    die "every requirement between humus modules must name $version at the release commit $sha: set them with release-modules.sh --bump $version and merge the change first"
  fi
  for entry in "${stale[@]}"; do
    read -r file path have <<<"$entry"
    echo "would set $file: require $path $version (now $have)"
  done
  # The tags would go on the commit that merges these changes, which does not
  # exist yet, so any tag that already exists is at a different commit.
  echo "release $version at the commit that sets these requirements on top of $sha"
  target=
else
  echo "release $version at $sha"
  target=$sha
fi

# The commit every existing tag on the remote points to, peeled through any
# annotated tag object.
remote_tags=$(git ls-remote --tags "$remote") || die "could not list the tags of $remote"
tagged_commit() {
  awk -v ref="refs/tags/$1" '
    $2 == ref { sha = $1 }
    $2 == ref "^{}" { peeled = $1 }
    END { print (peeled != "" ? peeled : sha) }
  ' <<<"$remote_tags"
}

refspecs=()
conflicts=0
for dir in "${modules[@]}"; do
  if [[ $dir == . ]]; then
    tag=$version
  else
    tag=$dir/$version
  fi
  existing=$(tagged_commit "$tag")
  if [[ -z $existing ]]; then
    refspecs+=("$sha:refs/tags/$tag")
    if [[ $mode == dry-run ]]; then
      echo "would create $tag"
    fi
  elif [[ $existing == "$target" ]]; then
    echo "exists $tag at the release commit, skipping"
  else
    echo "release-modules: $tag already exists at $existing, not at the release commit" >&2
    conflicts=1
  fi
done
[[ $conflicts == 0 ]] || exit 1

if [[ $mode == dry-run ]]; then
  echo "dry run: changed nothing"
  exit 0
fi

if [[ ${#refspecs[@]} -eq 0 ]]; then
  echo "nothing to create"
  exit 0
fi

git push --atomic "$remote" "${refspecs[@]}"
for refspec in "${refspecs[@]}"; do
  echo "created ${refspec#*:refs/tags/}"
done
