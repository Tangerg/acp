# Inspect or update the vendored schema

The machine-readable Agent Client Protocol contract, pinned to an upstream
release. It is a repository artifact rather than a build-time download, so the
wire grammar shows up in a diff when it moves.

## Files

| File | What it is |
| --- | --- |
| `schema.json` | Published stable v1 JSON Schema |
| `meta.json` | Method table: 13 agent methods, 11 client methods, and 1 protocol method |
| `manifest.json` | Generator roots described in [Choose generation roots](#choose-generation-roots) |
| `provenance.json` | Schema release, asset hashes, reference SDK commit and shared tool pins |

## Verify provenance

[`provenance.json`](./provenance.json) is the only editable source of the release
tag and asset hashes. The generator reads its schema tag, both recording scripts
read the same SDK commit, and CI verifies the vendored assets against the hashes.
The SDK package version is derived from the archived commit's `package.json`.

The assets are published by Zed Industries under the Apache License, Version 2.0,
in the [Agent Client Protocol repository](https://github.com/agentclientprotocol/agent-client-protocol).
The separate unstable and v2 assets are not inputs to this module.

## Update the schema pin

Moving the pin is a deliberate, reviewable commit, never a build step. Edit
`schema.tag` and `typescript.commit` in `provenance.json` to select a published
stable schema and a reference SDK that implements it. Fetch the selected assets:

```sh
tag=$(jq -er '.schema.tag' schema/provenance.json)
base=https://github.com/agentclientprotocol/agent-client-protocol/releases/download
curl -fsSL -o schema/schema.json "$base/$tag/schema.json"
curl -fsSL -o schema/meta.json "$base/$tag/meta.json"
shasum -a 256 schema/schema.json schema/meta.json
```

Record the two hashes in `schema.sha256` and `schema.metaSha256` of the same
provenance file. Then regenerate the Go model and both independent corpora:

```sh
go generate ./...
scripts/update-fixtures.sh
scripts/interop.sh
```

Commit the provenance, downloaded assets, generated output and recordings
together. If a release introduces a deserialization keyword, the generator must
implement it before the pin can move. If it changes a method, update the
capability table that CI compares with `meta.json`.

Two checks intentionally fail on a release that changes more than shapes.
`TestEveryNormativeClauseInTheSchemaIsClassified` requires a decision for each new
or removed obligation. The fixture corpus requires agreement with the independent
reference implementation, or an explicit, still-current schema-based divergence.
Changing an expected answer to match this implementation is not evidence.

## Choose generation roots

Generation scope is the transitive `$ref` closure of a committed root set, so
adding an operation is a diff in one file and the size of the exported surface is
an output CI checks rather than a number maintained in prose.

A definition is a root for one of four reasons, and the manifest keeps them
apart because they retire at different times:

- **`payloads`**: request, response, and notification payloads for implemented
  operations
- **`plumbing`**: types reached from the JSON-RPC envelope rather than from any
  method's params, which is how `Error` and `ErrorCode` become roots the moment
  `Error` is exported
- **`internal`**: protocol plumbing the connection needs but callers must not
  name; every entry records why it stays unexported
- **`probes`**: definitions rooted only to exercise a wire shape no implemented
  payload reaches yet. The list empties as the payload roots grow to reach the
  same shapes; every entry records the shape it proves

Projection sources are roots too. They currently duplicate payload roots, but
the generator derives that fact rather than relying on it.

## Licence and attribution

`schema.json` and `meta.json` belong to the specification. This repository
redistributes them under the Apache License, Version 2.0, as recorded in
[NOTICE](../NOTICE) and the [Apache 2.0 licence](https://www.apache.org/licenses/LICENSE-2.0).
