#!/usr/bin/env bash

# Both oracles execute a clean archive of the same commit. The caller owns the
# temporary directory; a supplied checkout is only a source of Git objects.
prepare_reference_sdk() {
	local project_dir="$1" temporary_dir="$2" source_checkout node_major schema_hash meta_hash tool
	local provenance="$project_dir/schema/provenance.json"
	for tool in git node npm jq shasum tar; do
		command -v "$tool" >/dev/null || {
			echo "reference-sdk: $tool is required" >&2
			return 1
		}
	done

	reference_repository=$(jq -er '.typescript.repository' "$provenance")
	reference_commit=$(jq -er '.typescript.commit | select(test("^[0-9a-f]{40}$"))' "$provenance")
	reference_schema=$(jq -er '.schema.tag' "$provenance")
	node_major=$(node -p 'process.versions.node.split(".")[0]')
	if [ "$node_major" != "$(jq -er '.tools.nodeMajor' "$provenance")" ]; then
		echo "reference-sdk: warning: Node $node_major differs from schema/provenance.json" >&2
	fi

	schema_hash=$(shasum -a 256 "$project_dir/schema/schema.json" | cut -d' ' -f1)
	meta_hash=$(shasum -a 256 "$project_dir/schema/meta.json" | cut -d' ' -f1)
	[ "$schema_hash" = "$(jq -er '.schema.sha256' "$provenance")" ] || {
		echo "reference-sdk: schema/schema.json does not match $reference_schema" >&2
		return 1
	}
	[ "$meta_hash" = "$(jq -er '.schema.metaSha256' "$provenance")" ] || {
		echo "reference-sdk: schema/meta.json does not match $reference_schema" >&2
		return 1
	}

	if [ -n "${ACP_TYPESCRIPT_SDK:-}" ]; then
		source_checkout="$ACP_TYPESCRIPT_SDK"
		git -C "$source_checkout" cat-file -e "$reference_commit^{commit}"
	else
		source_checkout="$temporary_dir/source"
		git init --quiet "$source_checkout"
		git -C "$source_checkout" remote add origin "$reference_repository"
		git -C "$source_checkout" fetch --quiet --depth 1 origin "$reference_commit"
	fi

	reference_checkout="$temporary_dir/sdk"
	mkdir "$reference_checkout"
	git -C "$source_checkout" archive "$reference_commit" | tar -x -C "$reference_checkout"
	reference_version=$(node -p 'require(process.argv[1]).version' "$reference_checkout/package.json")
	(cd "$reference_checkout" && npm ci --no-audit --no-fund >/dev/null)

	# The vendored stable schema owns this module's grammar. Regeneration keeps
	# the oracle independent without letting another SDK lane widen that grammar.
	cp "$project_dir/schema/schema.json" "$reference_checkout/schema/v1/schema.json"
	cp "$project_dir/schema/meta.json" "$reference_checkout/schema/v1/meta.json"
	(cd "$reference_checkout" && npm run generate -- --skip-download >/dev/null)
}
