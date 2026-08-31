#!/usr/bin/env bash
#
# release.sh — cut one release of this module.
#
# This is the executable form of "Releases" in CONTRIBUTING.md. The reasoning lives
# there; what lives here is the part a person should not be asked to remember at
# eleven at night.
#
# The organizing fact is that a published tag is immutable. The Go module proxy and
# the checksum database keep the first thing they saw under a version forever, so a
# tag pushed in error cannot be moved, deleted, or corrected — only superseded by a
# further release that everyone must then upgrade to. Everything this script does
# before the tag reaches the remote is undoable; nothing after it is. So the script
# is a dry run unless it is told otherwise, it verifies before it plans, and it
# prints the whole plan before it does any of it.
#
#   scripts/release.sh 0.2.0             # verify and print the plan
#   scripts/release.sh 0.2.0 --execute   # do it
#
# It never moves a tag, never force-pushes, and never leaves a `replace` directive
# in the module it is about to tag.

set -euo pipefail

GORELEASE=golang.org/x/exp/cmd/gorelease@v0.0.0-20260820142414-ca536658362e
REMOTE=origin

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

execute=false
version=""
for argument in "$@"; do
	case "$argument" in
	--execute) execute=true ;;
	-h | --help)
		sed -n '3,22p' "${BASH_SOURCE[0]}" | sed 's|^# \?||'
		exit 0
		;;
	-*)
		echo "release: unknown option: $argument" >&2
		exit 2
		;;
	*)
		if [[ -n "$version" ]]; then
			echo "release: give exactly one version" >&2
			exit 2
		fi
		version="$argument"
		;;
	esac
done

if [[ -z "$version" ]]; then
	echo "usage: scripts/release.sh X.Y.Z [--execute]" >&2
	exit 2
fi

# Accept the number and own the "v" ourselves, so a tag cannot be created as
# "vv0.2.0" by someone who typed the prefix out of habit.
version="${version#v}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "release: $version is not a semantic version" >&2
	exit 2
fi
tag="v$version"

# A tag is a promise about a specific tree. Refuse to make one about a tree that
# only exists on this machine.
if [[ -n "$(git status --porcelain)" ]]; then
	echo "release: the working tree has uncommitted changes" >&2
	exit 2
fi
if ! git remote get-url "$REMOTE" >/dev/null 2>&1; then
	echo "release: no '$REMOTE' remote; a release is a tag somebody else can fetch" >&2
	exit 2
fi
git fetch --tags --quiet "$REMOTE"
if git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null; then
	echo "release: $tag already exists; a published tag is never moved" >&2
	exit 2
fi
if grep -qE '^[[:space:]]*replace[[:space:]]' go.mod; then
	echo "release: go.mod contains a replace directive" >&2
	exit 2
fi

previous=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
base="${previous:-none}"

echo "release plan"
echo "  module:   $(go list -m)"
echo "  tag:      $tag"
echo "  baseline: $base"
echo "  commit:   $(git rev-parse --short HEAD)"
echo

# gorelease owns both halves of the promise: exported Go API compatibility and the
# module facts that make a release usable. Major zero is the explicitly unstable
# development period, so report every API change without applying stable-series
# compatibility policy to the version number.
echo "checking the public API against $base"
if [[ "$tag" == v0.* ]]; then
	go run "$GORELEASE" -base=none -version="$tag" >/dev/null
	go run "$GORELEASE" -base="$base"
else
	go run "$GORELEASE" -base="$base" -version="$tag"
fi
echo

if ! $execute; then
	echo "dry run. re-run with --execute to create and push $tag"
	exit 0
fi

git tag -a "$tag" -m "$tag"
git push "$REMOTE" "$tag"
echo "pushed $tag"
