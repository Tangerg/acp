# Roadmap

The order the package gets built in. Each layer is a product that works end to
end before the next one starts, per [AGENTS.md](../AGENTS.md): "Start from the
smallest version that works end to end, and add each new capability on top of a
product that already works."

This ordering was revised after
[design-review.md](./design-review.md#recommended-roadmap-changes). The change
that matters: what used to be one "wire" layer is now a spike followed by the
full generator. The schema turned out to have five union classes, 378
occurrences of `x-deserialize-default-on-error` and 35 of
`x-deserialize-skip-invalid-items`, so "generate all 265 definitions" was hiding
the possibility that the chosen Go representation cannot express the schema at
all. Finding that out on eight types is cheap; finding it out on 265 is a
rewrite.

## 1. Wire-semantics spike

Not a released layer — a question answered before the generator is written.
Hand-write or generate a handful of representative types and prove the model:

- one closed string enumeration and one open string union;
- one closed discriminated object union;
- one of the four open object unions, including its `not` exclusion and its
  retained extra payload;
- a required slice, which must encode as `[]` and never as `null`;
- a field carrying `x-deserialize-default-on-error`;
- an array carrying `x-deserialize-skip-invalid-items`;
- a type with real numeric or string constraints.

**Done when** accepted values, rejected values, default recovery and serialised
output all match the TypeScript SDK run against the same inputs. That
cross-check is the point: two endpoints from one Go implementation can agree
with each other and both be wrong.

## 2. Wire

The generator, for real.

- Vendor and pin both `schema.json` and `meta.json` at `schema-v1.21.0`.
- Generate the public payload types, the internal routing and envelope types, the
  method table, validation and codecs — with the visibility split in
  [design.md](./design.md#what-is-generated-and-what-is-exported), because
  `gorelease` will hold the module to whatever is exported at the first tag.
- Scope generation to the `$ref` closure of the operations layers 3–5 implement:
  156 of the 265 definitions. CI recomputes the closure and fails if the exported
  set differs, so growing the API is always a visible diff.
- Copy upstream's UNSTABLE marker into the doc comment of every symbol that
  carries one — 18 of them are inside that closure and cannot be avoided.
- Regeneration and the cross-SDK fixture comparison are both CI checks.

**Done when** the committed output is reproducible, the fixtures agree with the
TypeScript SDK, and the exported set equals the computed closure.

## 3. Link

The connection machinery, with no ACP semantics in it.

- `internal/jsonrpc2`, forked from `golang.org/x/tools/internal/jsonrpc2_v2`,
  upstream copyright and licence headers intact.
- `Transport` and `Connection`, with the concurrency and shutdown contract in
  [design.md](./design.md#transports) — not just the signatures.
- The connection state machine, and request-level cancellation via
  `$/cancel_request`.
- `InMemoryTransport` first, because it is what the later layers are tested with
  and it keeps `os` out of the package.

**Done when** a client and an agent complete `initialize` over an in-memory pipe,
and shutdown and cancellation are tested with `testing/synctest` rather than
sleeps.

## 4. Turn

The first release worth using.

- `session/new`, `session/prompt`, `session/update`,
  `session/request_permission`, behind the `*Session` handle.
- Turn cancellation as its own operation, separate from request cancellation —
  see [design.md](./design.md#two-cancellations-kept-apart).
- The error model: `*Error`, the eight codes, `ErrAuthRequired` reachable by
  `errors.Is`, and `-32800` mapped to `context.Canceled`. `auth_required` is on
  the ordinary path from `session/new`, so it cannot wait.
- `StdioTransport` and `CommandTransport`.

**Done when** this SDK interoperates with a TypeScript SDK process over stdio: a
full turn with a permission prompt in the middle, and a cancelled turn whose
final updates still arrive after the original caller has stopped waiting. Tag
`v0.1.0` only after that. Two Go endpoints talking to each other share any wire
bug they have and are not release evidence.

## 5. Workspace

- `fs/read_text_file` and `fs/write_text_file` — two independent capability
  booleans, so two independent handlers.
- The terminal group: all five methods together, because
  `ClientCapabilities.terminal` is one boolean covering all of them.
- The capability invariant enforced in both directions: construction fails if an
  advertised capability lacks its complete handler set, an inbound call to an
  unadvertised method is refused, and an outbound call the peer never advertised
  fails locally.

## 6. The rest

`session/load` and `session/resume`, modes and config options, `elicitation/*`,
the `mcp/*` passthrough, `providers/*`, `document/*`, `nes/*` — each only once
its capability-to-handler grouping is defined.

The extension boundary (`Call`, `Notify`, fallback handlers for `ExtRequest` /
`ExtNotification`) lands with layer 3, not here: it is how anyone implements an
ACP extension at all, and leaving it out would make the SDK a ceiling.

Not in this list, and deliberately:

- **Middleware.** The internal dispatch seam exists from layer 3 so it can be
  added without an API break. It stays unexposed until something needs it.
- **HTTP and WebSocket transports.** Work in progress upstream.
- **Schema v2.** Unstable upstream (`schema-v2.0.0-alpha.3`). If needed before it
  stabilises it goes in a separate package whose name says so.

## Open questions

Things that are not decided, listed so they are not mistaken for decided.

- **Where validation runs for large payloads.** Validating every inbound and
  outbound message is the correct default, but `session/update` arrives
  continuously for the whole of a turn. Whether that needs a fast path is a
  question for a benchmark, not for this document.
- **Whether `Session` survives a reconnect.** `session/load` and
  `session/resume` take a `SessionID` that outlives the connection it came from.
  Whether a `*Session` can be rebound to a new `ClientConn`, or whether callers
  keep the ID and ask for a fresh handle, decides how much state the handle
  holds. Layer 6 work, but it constrains the handle, so it should not drift.

### Resolved

- **v1's unstable subset.** Deferring it is not available, and the count is why.
  Of the 52 UNSTABLE definitions, 38 are unstable as types and 14 are stable
  types with unstable fields — including `SessionUpdate`, `PromptResponse`,
  `ToolCall` and both capability structs. Following every `$ref` from the 26
  method types layers 3–5 implement gives a closure of 156 of 265 definitions,
  and 18 of the 38 type-level-unstable types are inside it. So generation scope
  follows **reachability from implemented operations**, generated types are
  generated whole, and the module's compatibility promise carves out
  upstream-unstable symbols in writing before v1.0. See
  [design.md](./design.md#the-v1-schema-lane-only-which-is-not-the-same-as-stable).

  This reverses the recommendation offered before the numbers were computed.

- **Union decode helper.** No generic `As[T]` initially. Type switches and type
  assertions are standard Go and sufficient; a helper earns its place only if
  real callers show repeated logic it materially improves.
- **Ungated methods.** Refuse them, in both directions. Capabilities are an
  authority boundary, not presentation metadata. Extension methods stay on their
  own explicit fallback path.
- **Signals in `Agent.Run`.** A library does not own operating-system signals.
  `Run` stops when its context is cancelled or its transport ends; a `main`
  package owns `signal.NotifyContext`.
