#!/usr/bin/env bash
# Rewrites testdata/fixtures from the TypeScript SDK's own validators.
#
# The cross-check against the reference implementation is the point of the
# fixture corpus: two endpoints built from one Go implementation can agree with
# each other and both be wrong. But an oracle that runs on every build is a
# network dependency and a Node toolchain in a Go module's CI, so the answers are
# committed and this script is what produces them. `go test` replays the
# committed corpus with no network and no Node.
#
# Everything the answers depend on is pinned here. Without that, "the two SDKs
# agree" is an anecdote rather than release evidence.
#
# Run it when the schema pin moves, when a fixture is added, or on the schedule
# that catches upstream drift. The diff is the finding.
set -euo pipefail

# The reference implementation, pinned by commit rather than by version: the npm
# package does not export the validators this needs.
readonly SDK_REPOSITORY="https://github.com/agentclientprotocol/typescript-sdk"
readonly SDK_COMMIT="5dac09aaae3ebde1eaaf4a11840f7543f4806e20"
readonly SDK_VERSION="1.4.0"
# The schema release the SDK is generated from, which must be the one
# schema/README.md records.
readonly SCHEMA_TAG="schema-v1.21.0"
# Node's LTS line. The oracle only parses and re-serialises JSON, so the version
# is recorded for reproducibility rather than because anything here is new.
readonly NODE_MAJOR="22"

readonly root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	echo "update-fixtures: $*" >&2
	exit 1
}

command -v git >/dev/null || die "git is required"
command -v node >/dev/null || die "node is required"
command -v npm >/dev/null || die "npm is required"

node_major="$(node --version | sed 's/^v\([0-9]*\).*/\1/')"
if [ "$node_major" != "$NODE_MAJOR" ]; then
	echo "update-fixtures: warning: node $node_major, expected $NODE_MAJOR" >&2
fi

grep -q "$SCHEMA_TAG" "$root/schema/README.md" ||
	die "schema/README.md does not record $SCHEMA_TAG; the pins disagree"

# An existing checkout can be reused, which is what makes this runnable without
# the network. It still has to be the pinned commit: an oracle answering from
# some other revision is not the oracle this corpus claims.
if [ -n "${ACP_TYPESCRIPT_SDK:-}" ]; then
	checkout="$ACP_TYPESCRIPT_SDK"
	[ -d "$checkout" ] || die "ACP_TYPESCRIPT_SDK is not a directory: $checkout"
	head="$(git -C "$checkout" rev-parse HEAD)"
	[ "$head" = "$SDK_COMMIT" ] ||
		die "$checkout is at $head, not the pinned $SDK_COMMIT"
	cleanup() { :; }
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

cp "$root/scripts/oracle.ts" "$checkout/oracle.ts"
(cd "$checkout" && npx --no-install tsx oracle.ts "$root/testdata/fixtures")

echo "update-fixtures: done; review the diff in testdata/fixtures"
