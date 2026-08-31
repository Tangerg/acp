# Documentation

Design notes for a Go implementation of the Agent Client Protocol. Nothing here
is implemented yet; these are the decisions made before writing code, recorded so
that they can be argued with now rather than discovered later.

One page per reader task:

| Page | Answers |
| --- | --- |
| [protocol.md](./protocol.md) | What is ACP? What are the roles, the methods, and the shape of a turn? |
| [design.md](./design.md) | What does it look like in Go, and why that and not something else? |
| [design-review.md](./design-review.md) | Is the proposed Go design ready to implement, and what must change first? |
| [repository.md](./repository.md) | How is this repository built, and what does each gate prove? |
| [roadmap.md](./roadmap.md) | In what order does it get built? |

## Sources

Every factual claim in these pages comes from one of the following. Where a page
states a number — 43 methods, 265 definitions — it was counted from the vendored
schema, not recalled.

### The specification

- <https://agentclientprotocol.com> — the protocol site
- <https://agentclientprotocol.com/get-started/introduction> — what ACP is and the problem it solves
- <https://agentclientprotocol.com/protocol/overview> — roles, method directory, capability gating
- <https://github.com/agentclientprotocol/agent-client-protocol> — the specification repository, and where schema releases are published

The machine-readable contract is `schema.json` and `meta.json`, published as
assets on a release tag. The current stable tag is **`schema-v1.21.0`**; the
unstable line is `schema-v2.0.0-alpha.3`.

### The reference implementation

`~/Desktop/acp-typescript-sdk` — `@agentclientprotocol/sdk` v1.4.0, by Zed
Industries, Apache-2.0, at commit
`5dac09aaae3ebde1eaaf4a11840f7543f4806e20`. Upstream at
<https://github.com/agentclientprotocol/typescript-sdk>.

Line numbers below are pinned to that commit; both SDKs move them often, so check
the surrounding code rather than the number if they disagree.

The files these notes lean on:

| Path | What it gave |
| --- | --- |
| `schema/meta.json` | The method table: 28 agent, 14 client, 1 protocol |
| `schema/schema.json` | 265 definitions, 41 unions |
| `src/acp.ts:118` | `methods` — the method names grouped by area |
| `src/acp.ts:3723` | `interface Client` — what an editor must implement |
| `src/acp.ts:3890` | `interface Agent` — what an agent must implement |
| `src/acp.ts:2962` | `class TerminalHandle` — the ergonomics `terminal/*` wants |
| `scripts/generate.js:13` | The pinned schema release tags |
| `scripts/generate.js:450` | Schema download from the GitHub release |
| `package.json` (`generate:check`) | Regeneration as a CI check |

### The model for the Go shape

`~/Desktop/go-sdk` — `github.com/modelcontextprotocol/go-sdk`, the official MCP
SDK written by the Go team, at commit
`21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`. Upstream at
<https://github.com/modelcontextprotocol/go-sdk>.

It matters because it is the same problem already solved once in Go: a
bidirectional JSON-RPC protocol, capability-gated optional methods, a large
generated type set, and a spec that keeps moving. Its `design/design.md` argues
each choice rather than just stating it.

| Path | What it gave |
| --- | --- |
| `design/design.md:29` | Package layout — one package for the API |
| `design/design.md:45` | The `Transport`/`Connection` abstraction, and forking `jsonrpc2` |
| `design/design.md:239` | Protocol types generated from the schema; unions as interfaces |
| `design/design.md:279` | `Client`/`ClientSession` — why connections are n:1 |
| `design/design.md:371` | Why every spec method takes `(ctx, *Params)` and returns `(*Result, error)` |
| `design/design.md:405` | Middleware, and why it is an extension point rather than a feature |
| `mcp/transport.go:52` | The `Transport` interface as shipped |
| `mcp/content.go:22` | `Content` as a sealed interface with per-arm wire encoding |
| `mcp/client.go` | `ClientOptions` — handler fields that imply capabilities |

### The engineering setup

`~/Desktop/oolong` — `github.com/Tangerg/oolong`. The linter set, the CI gate,
the pinned tool versions and the release discipline in this repository were
ported from it. [repository.md](./repository.md) records what carried over
unchanged and what a single-module repository had to change.
