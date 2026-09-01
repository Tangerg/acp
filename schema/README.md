# The vendored schema

The machine-readable Agent Client Protocol contract, pinned to an upstream
release. It is a repository artifact rather than a build-time download, so the
wire grammar shows up in a diff when it moves.

## What is here

| File | What it is |
| --- | --- |
| `schema.json` | The v1 JSON Schema: 265 definitions, 41 unions. |
| `meta.json` | The method table: 28 agent, 14 client, 1 protocol. |
| `manifest.json` | The generator's roots. See [manifest.json](#manifestjson). |

## Provenance

| Fact | Value |
| --- | --- |
| Release tag | `schema-v1.21.0` |
| Published by | Zed Industries, under the Apache License, Version 2.0 |
| Upstream | <https://github.com/agentclientprotocol/agent-client-protocol> |
| Obtained from | `agentclientprotocol/typescript-sdk` at `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`, which downloads the release assets in `scripts/generate.js` |
| `schema.json` SHA-256 | `7f77702b34e0a0558e77220e9007bf8ee161a976bb8ac5021aba1b7e7b2c5708` |
| `meta.json` SHA-256 | `3026898232badf413624010d1343e20bef853e6705c62d6b56387cf9de6b0543` |

The unstable v2 line (`schema-v2.0.0-alpha.3`) is deliberately not vendored;
[docs/design.md](../docs/design.md#the-v1-schema-lane-only-which-is-not-the-same-as-stable)
says why.

## Moving the pin

Moving the pin is a deliberate, reviewable commit, never a build step:

```sh
tag=schema-v1.22.0
base=https://github.com/agentclientprotocol/agent-client-protocol/releases/download
curl -fsSL -o schema/schema.json "$base/$tag/schema.json"
curl -fsSL -o schema/meta.json "$base/$tag/meta.json"
shasum -a 256 schema/schema.json schema/meta.json   # record these above
go generate ./...                                   # regenerate; CI checks the diff
```

The commit updates the table above, the generated output, and — if the bump adds
or removes a method — the capability table, which CI compares against
`meta.json`.

## manifest.json

Generation scope is the transitive `$ref` closure of a committed root set, so
adding an operation is a diff in one file and the size of the exported surface is
an output CI checks rather than a number maintained in prose. See
[docs/design.md](../docs/design.md#a-root-manifest-not-a-number-in-three-documents).

A definition is a root for one of four reasons, and the manifest keeps them
apart because they retire at different times:

- `payloads` — request, response and notification payloads of implemented
  operations.
- `plumbing` — types reached from the JSON-RPC envelope rather than from any
  method's params, which is how `Error` and `ErrorCode` become roots the moment
  `Error` is exported.
- `markers` — types the extension boundary requires.
- `probes` — definitions rooted only to exercise a wire shape no implemented
  payload reaches yet. This list exists for the layer-1 spike described in
  [docs/roadmap.md](../docs/roadmap.md#1-wire-semantics-spike) and empties as the
  payload roots grow to reach the same shapes; every entry records the shape it
  is there to prove.

## Licence

`schema.json` and `meta.json` are the specification's, not this repository's.
They are redistributed under the Apache License, Version 2.0, as recorded in
[NOTICE](../NOTICE). A copy of the licence is at
<https://www.apache.org/licenses/LICENSE-2.0>.
