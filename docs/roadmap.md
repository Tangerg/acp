# Roadmap

The order the package gets built in. Each layer is a product that works end to
end before the next one starts, per [AGENTS.md](../AGENTS.md): "Start from the
smallest version that works end to end, and add each new capability on top of a
product that already works."

The layers are not equal in size. Layer 3 is the one that makes the module worth
publishing; layers 1 and 2 exist to make layer 3 possible, and layers 4 and 5 are
addition rather than invention.

## 1. Wire

The vendored schema, the generator, the types, the method constants. No I/O.

- `schema/schema.json` pinned to `schema-v1.21.0`.
- `internal/cmd/schemagen` emits the 265 definitions: structs, the 14 string
  enumerations, and the 27 struct unions as sealed interfaces with an unknown arm
  each.
- Method name constants from `meta.json`, so no method string is written twice.
- CI fails if regenerating changes the tree.

**Done when** every type round-trips, an unknown union arm round-trips unchanged,
and an unknown enumeration string survives decoding. Those three tests are the
whole point of the layer: they are what makes a schema bump a diff rather than an
incident. See [design.md](./design.md#unions).

## 2. Link

The connection machinery, with no ACP semantics in it.

- `internal/jsonrpc2`, forked from `golang.org/x/tools/internal/jsonrpc2_v2`.
- `Transport` and `Connection`.
- `InMemoryTransport` first, because it is what the rest of the layers are tested
  with and it keeps `os` out of the package.
- `Client`, `Agent`, `ClientConn`, `AgentConn` as types that can carry a request
  in either direction, and one internal dispatch handler so middleware can be
  added later without an API break.

**Done when** a client and an agent in one test complete `initialize` over an
in-memory pipe and each sees the other's capabilities.

## 3. Turn

The first release worth using: an editor can drive an agent and a user can
approve a tool call.

- `session/new`, `session/prompt`, `session/update`,
  `session/request_permission`.
- Cancellation as [design.md](./design.md#cancellation-is-not-ctxerr) describes
  it — `Prompt` sends `session/cancel` and keeps reading until the agent answers
  with `StopReason` `cancelled`.
- `StdioTransport` and `CommandTransport`, which is what makes it real against a
  published agent rather than only against itself.

**Done when** an example agent and an example client complete a full turn with a
permission prompt in the middle, and a cancelled turn still delivers the agent's
final updates. Tag `v0.1.0` here.

## 4. Workspace

The capability-gated methods that let an agent actually do work.

- `fs/read_text_file`, `fs/write_text_file`.
- `terminal/create`, `output`, `wait_for_exit`, `kill`, `release`, behind the
  `*Terminal` handle.
- Capability inference from handler fields, verified in both directions: a method
  whose capability was not advertised must be refused, and an advertised
  capability must have a handler.

**Done when** the SDK interoperates with a real published agent over
`CommandTransport` — the first evidence that comes from outside this repository.

## 5. The rest

Addition, in whatever order demand arrives: `session/load` and `session/resume`,
modes and config options, `elicitation/*`, the `mcp/*` passthrough,
`providers/*`, `document/*`, `nes/*`.

Not in this list, and deliberately:

- **Middleware.** The extension point stays unexposed until something needs it.
- **HTTP and WebSocket transports.** Work in progress upstream; implementing a
  moving target means implementing it twice.
- **Schema v2.** Unstable upstream (`schema-v2.0.0-alpha.3`). If it is needed
  before it stabilises it goes in a separate package whose name says so. See
  [design.md](./design.md#stable-only).

## Open questions

Things that are not decided, listed so they are not mistaken for decided.

- **Union decode ergonomics.** The sealed interface plus a type switch is
  settled. Whether to also ship a generic `As[T](SessionUpdate) (T, bool)` helper
  is not. It shapes every generated union, so it is worth answering before the
  generator is written rather than after.
- **How strictly to refuse an ungated method.** A peer that calls a method whose
  capability was never advertised is misbehaving, but returning an error is not
  obviously better than serving it. The strict reading is likely right; it needs
  checking against what real agents do.
- **Whether `Agent.Run` should own signal handling.** Convenient for the
  overwhelmingly common stdio agent, and the kind of thing a library usually
  should not take from its caller.
