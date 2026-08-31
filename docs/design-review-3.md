# Go design review, round 3

Review of [design.md](./design.md), [protocol.md](./protocol.md), and
[roadmap.md](./roadmap.md) after the third design revision.

**Status: approved to start implementation at Layer 1.** The wire-semantics
spike can begin now. It should be a small, real slice of the generator and codec
rather than disposable hand-written models. The exported API must not be frozen,
and Layers 3 and 4 must not be treated as directly implementable, until the
blocking items below are resolved.

This review is intentionally a new record. It does not edit the reviewed design
or the two earlier reviews.

> **Actioned.** Every finding was checked against the pinned schema and applied.
> Two notes for a later reader: the 357 optional-nullable count and the empty
> `_meta` responses reproduced exactly, and finding 4's link to
> `design.md#the-spike-before-the-generator` never resolved — the byte-identity
> promise it correctly identified lived in *Validation is part of the codec*,
> which now states four semantic assertions instead.

## Review basis

This pass checked the repository at
`7e321144209ad898baaa2960256d67997c080c7d` against:

- `~/Desktop/acp-typescript-sdk` at
  `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`, package version 1.4.0 and schema
  release `schema-v1.21.0`;
- `~/Desktop/go-sdk` at
  `21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`;
- the repository rules in [AGENTS.md](../AGENTS.md), especially that the schema
  owns the wire grammar, implementation should grow in working layers, and
  short-lived compatibility or replacement designs are not acceptable.

The TypeScript and Go SDK checkouts were clean at those revisions. All schema
claims below were reproduced from the pinned schema rather than inferred from
repository-local call sites.

## What is ready

The third revision resolves most of the structural problems from round 2 and
should keep these decisions:

- There is one authoritative API section rather than several competing sketches.
- `ClientSession` and `AgentSession` express protocol direction in the type
  system.
- Handle params mechanically omit only identifiers owned by the handle, while
  preserving every other request field.
- Handles are explicitly connection-bound, and session creation returns both a
  handle and the complete result.
- `ErrorCode` is the schema's open `int32`, remote `-32800` is kept distinct
  from local context completion, and code-based `errors.Is` is specified.
- Capabilities are a complete replacement rather than an ambiguous partial
  merge, and extension calls cannot use reserved standard method names.
- A committed root manifest replaces prose counts as the generator's source of
  truth.
- Authentication and cross-SDK interoperability are now on the pre-`v0.1.0`
  path.

Those are sound foundations. The remaining findings are about making their
implementation precise and sequencing them early enough.

## Conditions for Layer 1

### 1. Make the spike the first slice of the real generator

[The roadmap](./roadmap.md#1-wire-semantics-spike) currently permits
"Hand-write or generate". A hand-written type experiment can prove one encoding
idea while leaving the generator unable to reproduce it, and it creates code
whose planned replacement conflicts with this repository's long-term design
rule.

Start with a deliberately narrow but permanent vertical slice:

- pin `schema.json`, `meta.json`, provenance, and the upstream licence;
- create the real schema traversal and generation command;
- use a small, explicitly temporary-in-scope root manifest containing only the
  representative definitions;
- put shared recovery, validation, union, and JSON-value logic in the runtime
  location intended for the full generator;
- generate the representative Go types and run the semantic fixtures against
  them.

Layer 2 then grows the root manifest and generator coverage. It should not
replace the Layer 1 architecture.

### 2. Pin a reproducible TypeScript oracle before calling the spike complete

The local sibling checkout is useful evidence for this review, but a path under
`~/Desktop` cannot be a CI dependency. Installing
`@agentclientprotocol/sdk@1.4.0` is not sufficient by itself either: the package
exports its public runtime, experimental entry points, and schema JSON, but not
the generated Zod validators or schema-deserialization helpers used as the
oracle.

Choose and commit one reproducible mechanism before Layer 1 is complete:

- a pinned source checkout that is built and invoked through a supported local
  oracle script;
- a vendored, licence-preserving oracle subset at the pinned revision; or
- committed oracle outputs plus a pinned updater that is exercised in CI.

Whichever mechanism is chosen, record the exact SDK commit, npm package version,
schema release, Node version policy, and one command that regenerates the
fixtures. Otherwise "the two SDKs agree" is not repeatable release evidence.

### 3. Add omitted, null, and present to the representative cases

The design correctly says the wire grammar distinguishes omission from `null`,
but it does not define the Go representation that preserves that distinction.
A direct query of the pinned schema finds 357 optional nullable property
occurrences under `$defs`. A pointer with `omitempty` represents absent and null
with the same Go value, so ordinary generated pointers cannot round-trip this
part of the grammar.

Layer 1 must include at least one optional-nullable field and prove all three
states:

1. the property is omitted;
2. the property is present with `null`;
3. the property is present with a non-null value.

The spike should decide the reusable representation, such as a generated
presence-aware type with deliberate JSON methods. A state may be collapsed only
where the published schema explicitly makes the states equivalent. Also include
a required field with default-on-error: malformed input may recover to its
default, while a missing required property must still fail if that is the
TypeScript oracle's behaviour.

### 4. Use semantic preservation consistently; raw JSON bytes are not the contract

[design.md](./design.md#the-spike-before-the-generator) still promises that
canonical valid values round-trip byte-identically. The revised
[roadmap](./roadmap.md#1-wire-semantics-spike) correctly says the oracle is
semantic equivalence, but then retains exact-byte assertions for open object
unions and `_meta`. These statements are incompatible.

The schema does not make raw bytes part of `_meta`'s contract. The TypeScript SDK
parses `_meta` through `z.record(z.string(), z.unknown())`; its custom-payload
helper reattaches parsed JavaScript values, not the original whitespace, key
order, number spelling, or escape sequences. Cross-SDK tests therefore cannot
assert byte identity for `_meta` or the extra properties of an open union.

The Layer 1 acceptance rule should be:

- both SDKs accept the same valid inputs and produce semantically equivalent JSON;
- both SDKs apply schema-directed recovery to the same normalized value;
- values constructed in Go encode to stable Go-owned golden JSON;
- `_meta` and open-union extra properties survive as equivalent JSON values.

The Go implementation may retain `json.RawMessage` where that is the simplest
lossless representation, but raw-byte retention is then an implementation
property, not a schema or cross-SDK promise.

## Issues to resolve before freezing the exported wire surface

### 5. Separate public handle projections from internal wire requests

The handle rule says `PromptParams` is generated from `PromptRequest` minus the
bound `sessionId`, while the complete request remains the codec type. The
generation section separately says method params, results, and notifications
are exported from `acp`. Taken literally, that publishes both `PromptParams`
and `PromptRequest` even though only one is valid for ordinary callers.

Decide the visibility rule before expanding the Layer 2 root manifest. The
simplest surface is:

- export handle-facing projections and all domain and result types reachable
  from public operations;
- keep complete wire request types internal when their identifiers are supplied
  only by a handle;
- export a complete request only when it is itself part of a public handler
  contract and cannot be replaced by a direction-specific projection.

This decision depends on the missing agent handler signatures below. The
generator kernel can be built before that decision, but exported names and the
final public root manifest cannot be frozen.

### 6. Empty object responses still carry metadata and must not be discarded

The "response is returned, not absorbed" rule is correct, but the API violates
it for:

```go
func (*Terminal) Kill(context.Context, *KillTerminalParams) error
func (*Terminal) Release(context.Context, *ReleaseTerminalParams) error
```

`KillTerminalResponse` and `ReleaseTerminalResponse` are object responses with
optional `_meta`. Returning only `error` makes valid peer data unreachable.
These operations should return their generated result plus `error`, just like
other requests. Error-only methods are appropriate for notifications such as
`session/cancel` and `session/update`, not for JSON-RPC requests with schema
responses.

Audit every current and future empty-object response with the same rule. An
apparently empty response is not disposable when `_meta` is part of its schema.

The API table also mixes roadmap layers: it includes later `LoadSession` and
`SetMode` operations while omitting `ResumeSession`, session close and config,
and most other later operations. Either label the table as authoritative only
through a named layer and show exactly that surface, or complete it before an
API freeze. A partial table cannot simultaneously be the final authority.

## Issues to resolve before Layers 3 and 4

### 7. Specify the agent-side API and handler context

`ClientConfig` has a concrete sketch, but `AgentConfig` does not. `AgentConn`
has no methods, and the documents do not show how an inbound handler obtains the
`*AgentSession` needed to call `Update`, `RequestPermission`, filesystem, or
terminal operations. The symmetric extension contract is also incomplete:
`ClientConn` has `Call` and `Notify`, while `AgentConn` and both inbound fallback
handler signatures are absent.

Before the turn layer, specify at least:

- agent identity, `_meta`, authentication methods, capabilities, and logger;
- initialize, authenticate, new-session, prompt, and cancellation handlers;
- which handler receives `*AgentSession`, when that handle becomes valid, and
  whether one may escape the handler;
- baseline handler completeness checks at `NewAgent` and `NewClient`;
- `AgentConn` close, wait, peer-initialization snapshot, and extension methods;
- fallback extension handlers for both protocol directions.

Do not infer agent capability advertisement solely from the existence of client
callback methods. The config must make the implementation-to-advertisement
relationship explicit and construction must validate it.

### 8. Define client and agent connection milestones separately

The comment "Connecting performs initialize" cannot apply identically to both
sides. A client initiates `initialize`; an agent accepts it. The official MCP Go
SDK reflects that asymmetry: client connection performs its handshake, while
server connection starts serving and initialization occurs when the peer sends
the request.

Specify these milestones before implementing the Link API:

- `Client.Connect` returns only after transport connection, successful
  initialize, protocol-version validation, and storage of an immutable peer
  capability snapshot;
- if initialize fails, is cancelled, or negotiates an unsupported version,
  `Client.Connect` closes the logical connection before returning;
- `Agent.Connect` states whether it returns after starting the read loop or only
  after initialization, and which operations are legal before initialization;
- the context passed to `Connect` is either handshake/setup-scoped or owns the
  connection lifetime; it must not acquire both meanings accidentally;
- `Agent.Run` is defined in terms of connect, wait, and close, and its context
  clearly owns the run lifetime.

`Initialized()` should not expose mutable capability authority. Return a copy or
an immutable projection so a caller cannot mutate the same object used by
capability gates. Define `Wait` for local close, clean EOF, read/write failure,
concurrent callers, and repeated calls.

### 9. Correct and complete the cancellation ownership model

A Go context tree does not mean "each level is cancelled only by its own
signal." Parent cancellation cascades to descendants; child cancellation does
not cancel its parent or siblings. The implementation invariant should state
that rule directly.

Request cancellation should follow the complete MCP Go behaviour: once the call
context finishes, retire the local pending call, return the exact `ctx.Err()`,
send `$/cancel_request` with an independent bounded context, and consume or
discard any late response without reviving the call. The cited TypeScript SDK
does not itself prove immediate local return; it sends cancellation and still
settles the promise from the peer response.

Turn cancellation also needs directional ownership:

- when a client sends `session/cancel`, it synchronously claims the pending
  permission requests for that session so no late user decision can win after
  `Cancel` returns;
- when an agent receives `session/cancel`, it cancels the active turn and its
  descendant work without cancelling unrelated sessions;
- every pending permission response is completed exactly once with the schema's
  `cancelled` outcome.

Decide whether a session permits more than one active prompt. If it does not,
reject a concurrent prompt with a documented local or wire error. If it does,
define which turns `session/cancel` targets and how pending permission requests
are indexed. Do not leave this to scheduling accidents.

### 10. Move the complete capability table earlier

The version-pinned capability table is currently a Layer 5 deliverable, but
Layer 3 already exchanges capabilities and reserves every standard method name,
and Layer 4 needs authentication and session gating. Those layers cannot enforce
their advertised invariants without the table.

Commit the complete standard-method classification in Layer 2 or Layer 3. Every
method should be known as baseline, gated, or not yet implemented; later layers
activate rows without inventing the classification then. CI should compare the
table with every method in `meta.json`, even when most payload types are not yet
public generation roots.

### 11. Finish the custom-transport and logging contracts

A custom transport receives `jsonrpc.Message`, but the public `jsonrpc` package
does not yet say how an implementer encodes or decodes it. The official MCP Go
SDK publicly aliases its message abstraction and provides `MakeID`,
`EncodeMessage`, and `DecodeMessage`. ACP may intentionally expose less because
ordinary transports do not need to construct protocol envelopes, but an opaque
message still needs public encode/decode operations to support byte-stream
transports.

Choose the minimum usable surface before Layer 3. If planned HTTP or diagnostic
transports must inspect IDs, methods, requests, or responses, include those
requirements now rather than widening the package immediately after release.

Both config structs also need an explicit `*slog.Logger` policy. Plain handler
errors are promised to be logged locally, and notification handler failures have
no response channel. `nil` should have a safe, documented meaning, normally a
discarding logger. The library must never install an implicit stdout logger,
because an agent's stdout may be the protocol stream.

## Non-blocking API cautions

- Exported sentinel variables should not be mutable global `*Error` structs.
  Prefer exported `error` values backed by an immutable private code sentinel,
  while peer errors remain `*Error` values discoverable with `errors.As`.
- Add sentinels only for control-flow cases callers actually need. The open
  `ErrorCode` constants and `errors.As` cover ordinary inspection.
- Verify the `nil` params rule against the TypeScript oracle. The top-level ACP
  envelope may allow omitted or null params while an operation's object parser
  rejects `undefined`; the Go encoder may need to emit `{}` for an otherwise
  empty object rather than omit it.
- State whether session handles are safe for concurrent use independently of
  whether concurrent prompts are legal.

## Recommended first implementation slice

Implementation can start now with one bounded milestone:

1. Commit the pinned schema inputs, provenance, and reproducible TypeScript
   oracle mechanism.
2. Create the real generator command, a representative root manifest, and the
   shared codec runtime.
3. Cover every union class, required non-null slices, default-on-error,
   skip-invalid-items, constraints, and the omitted/null/present case.
4. Run semantic valid-input and recovery fixtures against the pinned TypeScript
   SDK, plus stable Go-owned encoder goldens.
5. Expand the generator only after the representation survives those tests.

In parallel with no public API commitment, the internal JSON-RPC link can be
ported with its licence, shutdown, late-response, and request-cancellation tests.
Do not publish connection or handle APIs merely because the internal link works.

The exit decision is therefore precise: **start Layer 1 now; after it passes,
continue into the private Layer 2 generator and codec kernel. Before freezing
the exported roots or claiming Layers 3 or 4 complete, resolve findings 5
through 11.**
