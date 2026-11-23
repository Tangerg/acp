#!/usr/bin/env bash

# Committed oracle answers keep ordinary Go builds independent of Node and the
# network. Only this recording step executes the reference implementation.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root
scratch="$(mktemp -d)"
readonly scratch
cleanup() { rm -rf -- "$scratch"; }
trap cleanup EXIT

source "$root/scripts/reference-sdk.sh"
prepare_reference_sdk "$root" "$scratch"

cp "$root/scripts/oracle.ts" "$reference_checkout/oracle.ts"
(cd "$reference_checkout" && node --import tsx oracle.ts "$root/testdata/fixtures")

echo "update-fixtures: recorded @agentclientprotocol/sdk@$reference_version ($reference_commit), $reference_schema"
