#!/usr/bin/env bash
# Rewrites testdata/fixtures from TypeScript SDK validators generated from the
# vendored published schema.
#
# The cross-check against the reference implementation is the point of the
# fixture corpus: two endpoints built from one Go implementation can agree with
# each other and both be wrong. But an oracle that runs on every build is a
# network dependency and a Node toolchain in a Go module's CI, so the answers are
# committed and this script is what produces them. `go test` replays the
# committed corpus with no network and no Node.
#
# The SDK supplies an independent implementation of ACP's lenient
# deserialisation rules. Its checked-in v1 schema is intentionally unstable, so
# using its validators verbatim would make experimental fields look stable.
# Generate fresh validators from this module's pinned published schema instead.
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
readonly SCHEMA_SHA256="caf62ff962ada396878372ced11efb2c6764e59d90919a38583c319948931a42"
readonly META_SHA256="061edb6efa8fb2aa2792459a86ec7268de5fe665bba48b2ffe7939df01481f88"
# Node's LTS line. The oracle only parses and re-serialises JSON, so the version
# is recorded for reproducibility rather than because anything here is new.
readonly NODE_MAJOR="22"

readonly root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly scratch="$(mktemp -d)"

cleanup() { rm -rf "$scratch"; }
trap cleanup EXIT

die() {
	echo "update-fixtures: $*" >&2
	exit 1
}

command -v git >/dev/null || die "git is required"
command -v node >/dev/null || die "node is required"
command -v npm >/dev/null || die "npm is required"
command -v shasum >/dev/null || die "shasum is required"

node_major="$(node --version | sed 's/^v\([0-9]*\).*/\1/')"
if [ "$node_major" != "$NODE_MAJOR" ]; then
	echo "update-fixtures: warning: node $node_major, expected $NODE_MAJOR" >&2
fi

grep -q "$SCHEMA_TAG" "$root/schema/README.md" ||
	die "schema/README.md does not record $SCHEMA_TAG; the pins disagree"
[ "$(shasum -a 256 "$root/schema/schema.json" | cut -d' ' -f1)" = "$SCHEMA_SHA256" ] ||
	die "schema/schema.json does not match the pinned $SCHEMA_TAG asset"
[ "$(shasum -a 256 "$root/schema/meta.json" | cut -d' ' -f1)" = "$META_SHA256" ] ||
	die "schema/meta.json does not match the pinned $SCHEMA_TAG asset"

# An existing checkout can be reused, which is what makes this runnable without
# the network. It still has to be the pinned commit: an oracle answering from
# some other revision is not the oracle this corpus claims.
if [ -n "${ACP_TYPESCRIPT_SDK:-}" ]; then
	source_checkout="$ACP_TYPESCRIPT_SDK"
	[ -d "$source_checkout" ] || die "ACP_TYPESCRIPT_SDK is not a directory: $source_checkout"
	head="$(git -C "$source_checkout" rev-parse HEAD)"
	[ "$head" = "$SDK_COMMIT" ] ||
		die "$source_checkout is at $head, not the pinned $SDK_COMMIT"
else
	source_checkout="$scratch/source"
	git init --quiet "$source_checkout"
	git -C "$source_checkout" remote add origin "$SDK_REPOSITORY"
	git -C "$source_checkout" fetch --quiet --depth 1 origin "$SDK_COMMIT"
fi

# Never install packages or generate sources in the user's reference checkout.
# Besides leaving dirt behind, doing so would make a second run depend on the
# first. A git archive gives every run the same clean input without copying
# node_modules or other untracked state.
checkout="$scratch/sdk"
mkdir "$checkout"
git -C "$source_checkout" archive "$SDK_COMMIT" | tar -x -C "$checkout"

version="$(node -p "require('$checkout/package.json').version")"
[ "$version" = "$SDK_VERSION" ] ||
	die "the checkout is @agentclientprotocol/sdk@$version, not the pinned $SDK_VERSION"

(cd "$checkout" && npm ci --no-audit --no-fund >/dev/null)

# The SDK deliberately consumes schema.unstable.json for its normal v1 build.
# Replacing that input before generation preserves its independently-written
# deserialisation machinery while making the published schema the sole wire
# authority.
cp "$root/schema/schema.json" "$checkout/schema/schema.json"
cp "$root/schema/meta.json" "$checkout/schema/meta.json"
(cd "$checkout" && npm run generate -- --skip-download >/dev/null)

cp "$root/scripts/oracle.ts" "$checkout/oracle.ts"
(cd "$checkout" && npx --no-install tsx oracle.ts "$root/testdata/fixtures")

echo "update-fixtures: done; review the diff in testdata/fixtures"
