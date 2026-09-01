#!/usr/bin/env bash
# Records this module's interoperability evidence against the reference
# implementation.
#
# Two Go endpoints talking to each other share any wire bug they have, so they are
# not release evidence. This runs the real thing: a subprocess built on
# agentclientprotocol/typescript-sdk, speaking newline-delimited JSON, driven by
# this module's client.
#
# The transcripts it writes are committed, and `go test` replays them with no
# network and no Node — the same arrangement as the fixture corpus, and for the
# same reason: an oracle that runs on every build is a network dependency and a
# Node toolchain in a Go module's CI. The recorded bytes are the reference
# implementation's, so replaying them still checks this package against another
# implementation rather than against itself.
#
# Run it when the schema pin moves, when the SDK pin moves, when a scenario is
# added, or on the schedule that catches upstream drift. The diff is the finding.
set -euo pipefail

# The same pins the fixture corpus uses. They are repeated rather than sourced
# because a pin that lives in one place and is read from two is a pin nobody
# notices moving.
readonly SDK_REPOSITORY="https://github.com/agentclientprotocol/typescript-sdk"
readonly SDK_COMMIT="5dac09aaae3ebde1eaaf4a11840f7543f4806e20"
readonly SDK_VERSION="1.4.0"
readonly SCHEMA_TAG="schema-v1.21.0"
readonly NODE_MAJOR="22"

readonly root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	echo "interop: $*" >&2
	exit 1
}

command -v git >/dev/null || die "git is required"
command -v node >/dev/null || die "node is required"
command -v npm >/dev/null || die "npm is required"
command -v go >/dev/null || die "go is required"

node_major="$(node --version | sed 's/^v\([0-9]*\).*/\1/')"
if [ "$node_major" != "$NODE_MAJOR" ]; then
	echo "interop: warning: node $node_major, expected $NODE_MAJOR" >&2
fi

grep -q "$SCHEMA_TAG" "$root/schema/README.md" ||
	die "schema/README.md does not record $SCHEMA_TAG; the pins disagree"
grep -q "$SDK_COMMIT" "$root/scripts/update-fixtures.sh" ||
	die "the fixture updater pins a different SDK commit; the two oracles must be one"

# An existing checkout can be reused, which is what makes this runnable without
# the network. It still has to be the pinned commit.
if [ -n "${ACP_TYPESCRIPT_SDK:-}" ]; then
	checkout="$ACP_TYPESCRIPT_SDK"
	[ -d "$checkout" ] || die "ACP_TYPESCRIPT_SDK is not a directory: $checkout"
	head="$(git -C "$checkout" rev-parse HEAD)"
	[ "$head" = "$SDK_COMMIT" ] ||
		die "$checkout is at $head, not the pinned $SDK_COMMIT"
	cleanup() { rm -f "$checkout/interop-agent.ts"; }
else
	checkout="$(mktemp -d)"
	cleanup() { rm -rf "$checkout"; }
	git init --quiet "$checkout"
	git -C "$checkout" remote add origin "$SDK_REPOSITORY"
	git -C "$checkout" fetch --quiet --depth 1 origin "$SDK_COMMIT"
	git -C "$checkout" checkout --quiet FETCH_HEAD
fi
trap cleanup EXIT

version="$(node -p "require('$checkout/package.json').version")"
[ "$version" = "$SDK_VERSION" ] ||
	die "the checkout is @agentclientprotocol/sdk@$version, not the pinned $SDK_VERSION"

(cd "$checkout" && npm ci --no-audit --no-fund >/dev/null)
cp "$root/scripts/interop-agent.ts" "$checkout/interop-agent.ts"

mkdir -p "$root/testdata/interop"
# The agent runs from inside the checkout, so that Node resolves the SDK's own
# dependencies rather than this repository's absence of them.
go run -C "$root" ./internal/cmd/interop \
	-dir "$root/testdata/interop" \
	-sdk-commit "$SDK_COMMIT" \
	-sdk-version "$SDK_VERSION" \
	-schema "$SCHEMA_TAG" \
	-- "$checkout/node_modules/.bin/tsx" "$checkout/interop-agent.ts"

echo "interop: done; review the diff in testdata/interop"
