# Inspect or update the vendored schema

The machine-readable Agent Client Protocol contract, pinned to an upstream
release. It is a repository artifact rather than a build-time download, so the
wire grammar shows up in a diff when it moves.

## Files

| File | What it is |
| --- | --- |
| `schema.json` | Published stable v1 JSON Schema: 170 definitions and 32 unions |
| `meta.json` | Method table: 13 agent methods, 11 client methods, and 1 protocol method |
| `manifest.json` | Generator roots described in [Choose generation roots](#choose-generation-roots) |

## Verify provenance

| Fact | Value |
| --- | --- |
| Release tag | `schema-v1.21.0` |
| Published by | Zed Industries, under the Apache License, Version 2.0 |
| Upstream | [Agent Client Protocol repository](https://github.com/agentclientprotocol/agent-client-protocol) |
| Obtained from | The two assets attached to the upstream `schema-v1.21.0` release |
| `schema.json` SHA-256 | `caf62ff962ada396878372ced11efb2c6764e59d90919a38583c319948931a42` |
| `meta.json` SHA-256 | `061edb6efa8fb2aa2792459a86ec7268de5fe665bba48b2ffe7939df01481f88` |

The TypeScript checkout also contains experimental additions under the same v1
tag. They are deliberately not copied: the release assets are the published wire
contract, and repository-local SDK additions cannot widen it.

## Update the schema pin

Moving the pin is a deliberate, reviewable commit, never a build step:

Set the new release tag, download both assets, and record their hashes:

```sh
tag=schema-v1.22.0
base=https://github.com/agentclientprotocol/agent-client-protocol/releases/download
curl -fsSL -o schema/schema.json "$base/$tag/schema.json"
curl -fsSL -o schema/meta.json "$base/$tag/meta.json"
shasum -a 256 schema/schema.json schema/meta.json
go generate ./...
```

Update the provenance table with the new hashes. Commit the downloaded assets and
generated output together. If the release adds or removes a method, also update
the capability table that continuous integration (CI) compares with `meta.json`.

Two tests fail on a release that changes more than shapes, and both are meant to.
`TestEveryNormativeClauseInTheSchemaIsClassified` fails when the prose states an
obligation nothing has answered, or when a row answers one upstream has dropped;
the fixture corpus fails where the reference implementation and this package now
disagree. Neither is satisfied by editing the expectation — the first wants a
reading of the new clause, and the second wants `scripts/update-fixtures.sh`.

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
