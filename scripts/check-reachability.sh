#!/usr/bin/env bash

# Every callable path needs executable evidence. For private functions, a deadcode
# finding is ordinary dead implementation. For an exported operation in a library,
# it means the public contract lacks a caller-side test or example — never that the
# operation is unnecessary. Public API is retained or removed only after reviewing
# its responsibility and abstraction independently of repository call counts.

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

command -v deadcode >/dev/null || {
	echo "deadcode is required; install the pinned version from CONTRIBUTING.md" >&2
	exit 2
}

failed=false
for goos in linux darwin windows; do
	findings=$(GOOS="$goos" CGO_ENABLED=0 deadcode -test ./...)
	if [[ -n "$findings" ]]; then
		printf '%s:\n%s\n' "$goos" "$findings" >&2
		failed=true
	fi
done

if $failed; then
	exit 1
fi
