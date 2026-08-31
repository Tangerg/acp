# Go design review, round 2

Review of [design.md](./design.md), [protocol.md](./protocol.md), and
[roadmap.md](./roadmap.md) after the second design revision.

> **Actioned.** Every finding below was checked against the schema and applied.
> Section links into `design.md` point at the revision reviewed here
> (`34789f4`) and several no longer resolve, because finding 1 asked for the
> competing API sketches to be consolidated into one and they were: `Client and
> agent`, `Sessions are a handle…`, `Two cancellations, kept apart` and `Who owns
> initialization` are now `The API` plus the prose sections that refer to it.

**Status: ready for the wire-semantics spike, but not ready to freeze the public
API or begin the connection and turn layers.** The first review's major protocol
findings have been addressed, and the architecture remains a good basis for the
SDK. The remaining problems are narrower: the documents now describe several
incompatible versions of the public API, the new handles discard schema-visible
data, and the error, capability, cancellation, and generation contracts still
have gaps that would otherwise become implementation policy by accident.

## Review basis

This pass checked the current repository at
`34789f4526ec1e4b65a445e936d3089a1782ee48` against:

- `~/Desktop/acp-typescript-sdk` at
  `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`, package version 1.4.0 and schema
  release `schema-v1.21.0`;
- `~/Desktop/go-sdk` at
  `21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`;
- the repository rules in [AGENTS.md](../AGENTS.md), especially that the
  published schema owns the wire grammar and related construction settings use
  explicit configuration structs.

The count behind the revised roadmap was reproduced directly from the schema.
The 25 method payload definitions currently named by layers 3 to 5 have a
transitive `$ref` closure of 156 definitions, including 18 definitions whose own
description carries the upstream unstable warning.

## What the revision fixed

The following changes resolve the corresponding findings from the first review
and should stay:

- The 41 unions are classified by their actual schema shapes. Closed enums and
  closed object unions no longer receive invented unknown arms.
- Schema-directed recovery, validation, required slices, and the two
  `x-deserialize-*` keywords are part of the codec design and the first layer is
  now a representative spike rather than a full generator.
- Request cancellation and turn cancellation are separate operations.
- Terminal support is one complete handler group, filesystem support remains two
  independent capabilities, and unsupported calls are refused in both
  directions.
- The extension-method boundary, generated visibility split, transport
  concurrency contract, and initialization owner are now stated.
- TypeScript interoperability moved before `v0.1.0`.
- The v1 lane is no longer described as wholly stable, and the Go floor now has
  a concrete reason rather than following the newest toolchain.
- The previous open questions about union helpers, ungated calls, and signal
  ownership have sensible answers.

## Issues that block the public API

### 1. The documents still specify three incompatible connection APIs

[Client and agent](./design.md#client-and-agent) still publishes these methods:

```go
func (*ClientConn) Initialize(context.Context, *InitializeParams) (*InitializeResult, error)
func (*ClientConn) NewSession(context.Context, *NewSessionParams) (*NewSessionResult, error)
func (*ClientConn) Prompt(context.Context, *PromptParams) (*PromptResult, error)
func (*ClientConn) Cancel(context.Context, *CancelParams) error
```

[Sessions are a handle](./design.md#sessions-are-a-handle-for-the-same-reason-terminals-are)
then changes `NewSession` to return `*Session` and explicitly says that
`ClientConn.Prompt` does not exist. [Two cancellations, kept
apart](./design.md#two-cancellations-kept-apart) subsequently reintroduces
`ClientConn.Prompt` and `ClientConn.Cancel`. Finally, [Who owns
initialization](./design.md#who-owns-initialization) removes the public
`Initialize` operation and makes `Client.Connect` perform it.

These are mutually exclusive contracts, not illustrative variations. There
should be one authoritative API table after all design decisions, and later
sections should refer to it rather than restating signatures.

Initialization also needs an input owner. The proposed `Client.Connect(ctx, t)`
has no `InitializeParams`, while the schema requires a protocol version and
allows per-connection client information and `_meta`. Put the fixed values in
`ClientConfig`, or add an explicit `ConnectConfig` whose zero value selects the
supported protocol version; do not leave `Connect` to invent them.

The construction contract has the same mismatch. The capability section says
invalid configurations fail during construction, but `NewClient(*ClientConfig)
*Client` has no error result. Either make invalid states unrepresentable through
the configuration shape or return an error. A configuration error should not be
deferred until an inbound request discovers it.

### 2. The new handles bind IDs but currently discard the rest of the schema

The handle idea is appropriate, but its current signatures are not lossless.

`NewSessionResponse` contains `sessionId`, `modes`, `configOptions`, and `_meta`.
Returning only a `*Session` that, by definition, stores just a connection and an
ID makes the other three fields unreachable. `LoadSessionResponse` and
`ResumeSessionResponse` also return mode, configuration, and metadata state.
The TypeScript SDK's `ActiveSession` retains the complete
`NewSessionResponse` and exposes it rather than reducing it to the ID
(`src/acp.ts:768-824`). The Go design needs an equally explicit answer: retain an
immutable initial response on the handle, return the response alongside the
handle, or return a result object containing both.

The request side has the inverse problem. `PromptRequest`,
`SetSessionModeRequest`, and the other session-scoped wire types already require
`sessionId`. If a `Session` method accepts the generated request unchanged,
there are two IDs and they can disagree. If it omits params entirely, schema
fields disappear. In particular:

- `Session.Cancel(ctx)` and `Session.Close(ctx)` cannot send `_meta`;
- `Terminal.Output(ctx)`, `Kill(ctx)`, `WaitForExit(ctx)`, and `Release(ctx)`
  cannot send their request `_meta`;
- `Session.Prompt(ctx, *PromptParams)` is ambiguous about whether its params
  still contain the already-bound ID.

Define a mechanical projection rule for handle methods: bind the owned
identifiers internally, while an API params struct retains every other schema
field, including `_meta`. Keep the generated wire request as the codec type. Do
not accept two independent copies of the same identifier.

The handle direction must also be represented in the type system. A client-side
session calls `Prompt`, `Cancel`, and lifecycle methods on an agent. An
agent-side session calls `RequestPermission`, filesystem methods, and terminal
methods on a client. One concrete `Session` exposing both sets permits calls
that can never be valid for the side holding it. Separate direction-specific
handles, such as `ClientSession` and `AgentSession`, keep that error out of the
runtime.

Finally, the supporting count should say that 32 **definitions** have a direct
`sessionId` property. They are not all request types: the set includes
`NewSessionResponse`, `ForkSessionResponse`, `SessionInfo`,
`ElicitationSessionScope`, and several notifications. The corrected count still
supports a handle; it should not be given a stronger label than the query proves.

### 3. The error model is narrower than `ErrorCode` and wider than Go context semantics

The schema defines eight **predefined constants**, followed by an unrestricted
`int32` arm named `Other`. The TypeScript type is correspondingly the eight
literals plus `number`, and its validator enforces the `int32` range
(`src/schema/types.gen.ts:3605-3614`,
`src/schema/zod.gen.ts:2308-2325`). Therefore:

- the prose should say “eight predefined codes,” not that `ErrorCode` has eight
  possible values;
- unknown in-range codes must survive;
- `Error.Code int64` is too broad unless every construction and encoding path
  validates the range. A named `ErrorCode int32` expresses the schema directly.

The unconditional mapping of remote `-32800` to `context.Canceled` is also
incorrect. The schema says that code may result from caller cancellation,
resource constraints, **or shutdown**. It does not prove that the local context
was cancelled. The Go rule should be:

1. If the call context is done, return its actual `ctx.Err()`. A deadline remains
   `context.DeadlineExceeded`; an explicit cancellation remains
   `context.Canceled`.
2. If the context is still live and the peer returns `-32800`, preserve the
   `*Error`, optionally making it match an `ErrRequestCancelled` code sentinel.

`ErrAuthRequired` also needs a specified mechanism. Wrapping `*Error` makes
`errors.As` work, but does not by itself make
`errors.Is(err, ErrAuthRequired)` work. Define `(*Error).Is` to compare error
codes, or wrap a dedicated sentinel while preserving the wire error. The same
rule should cover all code sentinels consistently.

Finally, a plain handler error should not automatically send `err.Error()` to the
peer. Such messages commonly contain paths and implementation details. Log the
detailed local error and send a stable “Internal error” message. A handler that
deliberately returns `*Error` may control the peer-visible code, message, and
data.

### 4. Turn cancellation still lacks a session-level ownership model

Separating `$/cancel_request` from `session/cancel` fixed the public meaning, but
the implementation obligations are not yet assigned to an owner.

When a client sends `session/cancel`, the schema requires it to answer every
pending `session/request_permission` request for that session with the
`cancelled` outcome. The user handler may currently be blocked waiting for UI.
The connection therefore needs to index pending callbacks by session, cancel
their handler contexts, resolve the response exactly once, and handle the race
with a user decision. This cannot be delegated to an application after
`Session.Cancel` has already returned.

On the agent side, receiving `session/cancel` must signal the active prompt-turn
context without cancelling unrelated work on the connection. Conversely,
receiving `$/cancel_request` may cancel that request and nested activity, but it
does not promise the `session/cancel` result shape. The design should state the
context tree and the one owner of each pending turn and callback.

The nil permission-handler policy is not a safe substitute. The wire outcome is
either `cancelled` or a selected option ID; there is no universal “deny” outcome,
and an agent is not required to offer a reject option. Because
`session/request_permission` is baseline client behaviour, a missing handler
should make client construction fail rather than fabricate a response.

### 5. Capability enforcement has no machine-readable source or complete merge rule

The strict capability invariant is the right policy, but “whatever the schema's
capability types say” is not enough to implement it. The schema annotates method
payloads with `x-method` and `x-side`; it has no annotation linking a method to a
capability predicate. Those links currently live only in descriptions, and some
features such as prompt content support describe accepted data rather than an
optional method.

Add a version-pinned capability table that records, for every standard method:

- whether it is baseline or gated;
- the direction in which the check applies;
- the exact capability predicate;
- the complete local handler group required to advertise it.

Generation or CI can verify that the table covers every method in `meta.json`
and contains no removed method. Focused tests should prove each non-trivial
predicate, especially terminal, filesystem, authentication, session lifecycle,
elicitation, and MCP.

The meaning of `Capabilities *ClientCapabilities` must also be exact. Calling it
a partial refinement is ambiguous for scalar booleans: `false` is both a useful
wire zero and the Go value for “not set.” The simplest coherent rule is:

- `nil` means derive the complete advertisement from handlers and group config;
- non-nil means a complete desired advertisement, not a partial patch;
- construction rejects any desired capability unsupported by the configured
  implementation.

Alternatively, put the refinements inside each handler group and remove the raw
override. Do not define a field-by-field merge that needs a third boolean state
the generated capability type cannot represent.

### 6. The 156-definition root set does not match the promised surface

The number 156 is reproducible, but the description of it is not. Layers 3 to 5
currently name 25 method payload definitions, not 26, and those 25 produce the
156-definition closure.

That closure excludes the schema's `Error` and `ErrorCode` definitions because
errors are reached from JSON-RPC response envelopes, not from method params and
results. Yet the revised design explicitly exports `Error`. Adding that root
makes the closure 158.

Layer 4 also describes authentication-required as an ordinary session-creation
path but does not include the `authenticate` method. A `v0.1.0` client that can
recognize `ErrAuthRequired` but cannot perform `authenticate` is not end-to-end
against an agent that requires authentication. Adding `AuthenticateRequest` and
`AuthenticateResponse` to the current method roots also makes the closure 158;
adding both authentication and the public error roots makes it 160.

Replace the prose count with a committed root manifest derived from the actual
implemented public API. It should separately name:

- implemented request, response, and notification payloads;
- protocol plumbing needed by those operations, including errors and
  cancellation;
- any generated marker types required by the extension boundary.

CI should compute the closure from that manifest. A count is then an output to
check, not a second source of truth to maintain in three documents.

### 7. The generic extension API can bypass every standard-method invariant

`Call(ctx, method, params, result)` and `Notify(ctx, method, params)` accept any
string. As written, callers can pass `session/prompt`, `fs/read_text_file`, or
another standard name and bypass the generated params type, outbound validation,
session-ID binding, and capability checks. A fallback handler can similarly
intercept a misspelled or otherwise unavailable standard operation.

Reserve the complete generated standard-method set. The generic extension API
should reject those names and fallback handlers should run only for names outside
that set. Standard methods must have exactly one path through the typed codec and
capability gate. If raw access to a standard method is ever required for a
diagnostic tool, it should be a separately named unsafe API rather than a side
effect of ordinary extension support.

### 8. The wire-spike oracle promises byte identity that JSON decoding cannot preserve

The design says canonical valid values round-trip byte-identically. Unless a
canonical JSON encoding is defined first, that is not a property of
`encoding/json` or of the TypeScript SDK. Whitespace, object-key order, number
spelling, escapes, default insertion, and valid unknown object properties can all
change while the decoded value remains equivalent.

Use three distinct assertions:

1. Valid input is accepted by both SDKs and normalizes to semantically equivalent
   JSON values.
2. Malformed input subject to schema recovery produces the same normalized value
   in both SDKs.
3. The Go encoder has exact golden output for values constructed in Go.

Exact raw retention remains appropriate only where the schema explicitly makes
it part of the contract, such as the payload of an open object-union arm and
`_meta` values.

The final “not copied from TypeScript” section should also agree with the earlier
codec section. It currently says that exceptional cases get a hand-written
check, while the codec section promises generated validation. At this schema
size, schema constraints and recovery rules should be generated with a small
shared runtime; hand-written validation should be reserved for SDK invariants
that are not expressible in the schema.

## Recommended decisions before layer 2

1. Publish one definitive API sketch after applying initialization ownership and
   direction-specific session handles; delete the superseded sketches from the
   design when the review documents are retired.
2. Make handles bind only their IDs. Preserve every remaining request field and
   every response field through explicit params and result contracts.
3. Make session handles connection-bound. `LoadSession` and `ResumeSession`
   create a new handle on the new connection; callers retain a `SessionID`, not a
   handle that silently changes its transport.
4. Use an open, range-correct error-code type; distinguish local context
   completion from remote cancellation; specify code-based `errors.Is`.
5. Add the session-turn state machine, including pending permission completion,
   to layer 4's done criteria.
6. Make capability gating a checked table and choose either full capability
   replacement or group-local refinement.
7. Drive generation from an explicit root manifest, and include authentication
   in the first interoperable release.
8. Reserve standard method names from the extension path and replace byte-level
   cross-SDK round trips with normalized semantic comparisons.

With those decisions recorded, the plan is implementable in layers without
changing its core architecture. The wire-semantics spike can start now because
none of these findings changes the five union classes or the schema-recovery
model it is meant to prove.
