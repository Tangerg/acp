#!/usr/bin/env bash

# A real subprocess on the reference SDK catches wire mistakes two Go endpoints
# could share. The committed transcript replays without Node or the network.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root
scratch="$(mktemp -d)"
readonly scratch
cleanup() { rm -rf -- "$scratch"; }
trap cleanup EXIT

source "$root/scripts/reference-sdk.sh"
prepare_reference_sdk "$root" "$scratch"

cp "$root/scripts/interop-agent.ts" "$reference_checkout/interop-agent.ts"
mkdir -p "$root/testdata/interop"
go build -C "$root" -o "$scratch/interop" ./internal/cmd/interop
(cd "$reference_checkout" && "$scratch/interop" \
	-dir "$root/testdata/interop" \
	-sdk-commit "$reference_commit" \
	-sdk-version "$reference_version" \
	-schema "$reference_schema" \
	-- node --import tsx interop-agent.ts)

echo "interop: done; review the diff in testdata/interop"
