# Go design review

Review of [design.md](./design.md), [protocol.md](./protocol.md), and
[roadmap.md](./roadmap.md) before implementation begins.

**Status: Issues found.** The overall architecture is sound, but the wire-type,
cancellation, capability, and extension contracts need revision before layer 1
is implemented. Following the current text would create behavior that disagrees
with the published schema and would likely force the wire and turn layers to be
rewritten.

## Review basis

This review checked the documents against these local references:

- `~/Desktop/acp-typescript-sdk` at
  `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`, package version 1.4.0,
  schema release `schema-v1.21.0`.
- `~/Desktop/go-sdk` at
  `21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`, the official MCP Go SDK.
- The repository rules in [AGENTS.md](../AGENTS.md), in particular that the
  published ACP schema wins over a locally preferred wire grammar.

The method and definition counts in the documents are correct: `meta.json` has
28 agent methods, 14 client methods, and one protocol method, while
`schema.json` has 265 definitions and 41 top-level unions. The problem is the
classification and intended decoding of those definitions, not the counts.

## What should stay

The following decisions are appropriate and should remain the basis of the
implementation:

- Keep the user-facing API in one `acp` package, with only JSON-RPC message
  types needed by custom transports in a separate public `jsonrpc` package.
- Keep the connection engine and generator internal.
- Vendor and pin both `schema.json` and `meta.json`, commit generated output,
  and make regeneration a clean-tree CI check.
- Separate `Client` and `Agent` configuration from `ClientConn` and
  `AgentConn`; one configured endpoint may own multiple connections.
- Reserve *session* for an ACP conversation and use *connection* for the
  JSON-RPC link.
- Represent a terminal as a handle with an explicit `Release` obligation.
- Defer middleware, HTTP/WebSocket transports, and schema v2 until a concrete
  requirement or stable specification exists.
- Build in end-to-end layers rather than exposing the entire protocol in the
  first release.

## Issues that block implementation

### 1. The union model disagrees with the v1 schema

The documents divide the 41 unions into 14 open string enumerations and 27
discriminated struct unions. The actual schema does not have that shape.

The 14 enumerations, including `StopReason`, are closed `oneOf` sets of string
constants. The TypeScript SDK's generated Zod validators reject unknown values.
Keeping an unknown string during Go unmarshalling would therefore be more
permissive than the published grammar. If forward compatibility requires an
open value, that change belongs upstream in the schema.

The remaining 27 unions are not all struct unions. They include:

- primitive unions such as `RequestId` (`string`, integer, or `null`);
- integer unions such as `ErrorCode`;
- value unions such as `ElicitationContentValue`;
- explicitly open strings such as `LlmProtocol` and
  `SessionConfigOptionCategory`;
- closed discriminated object unions such as `SessionUpdate`;
- open object unions with a schema-defined catch-all.

Only four v1 unions have the schema-defined open catch-all recognized by the
TypeScript generator:

- `CreateElicitationRequest`;
- `CreateElicitationResponse`;
- `ElicitationPropertySchema`;
- `MultiSelectItems`.

`SessionUpdate` is a closed 15-arm union in v1. Adding
`UnknownSessionUpdate`, or adding an unknown arm to every object union, invents
a local dialect.

The generator must classify unions by their actual wire shape:

1. Closed string enums use a named string type and reject unknown wire values.
2. Open string unions use a named string type and retain unknown values.
3. Closed discriminated object unions use a sealed interface and reject an
   unknown discriminator.
4. The four open object unions use a sealed interface with a raw catch-all arm
   that implements the schema's `not` exclusion and preserves its extra
   payload.
5. Primitive and mixed unions receive representations specific to their JSON
   shapes rather than being forced through an object-interface template.

### 2. Generated structs plus occasional checks do not implement the grammar

`encoding/json` checks JSON syntax and basic Go field types. It does not enforce
the rest of this schema, including:

- required fields, constants, enums, numeric bounds, lengths, and formats;
- the distinction between omitted, `null`, and present values where it matters;
- required slices, for which a nil Go value otherwise encodes as invalid
  `null` rather than an array;
- `x-deserialize-default-on-error`;
- `x-deserialize-skip-invalid-items`;
- the `not` and extra-payload rules of open unions.

The v1 schema currently contains 378 occurrences of
`x-deserialize-default-on-error` and 35 occurrences of
`x-deserialize-skip-invalid-items`. The TypeScript SDK has dedicated generator
resolvers and runtime helpers for these semantics. Describing the Go version as
ordinary decoding plus a few hand-written checks leaves too much of the wire
contract unspecified.

Before generating all definitions, layer 1 needs a small representative spike
covering:

- one closed enum;
- one open string;
- one closed discriminated union;
- one of the four open unions;
- a required slice;
- a field that defaults on malformed input;
- an array that skips malformed items;
- a type with meaningful numeric or string constraints.

The design must then state where validation happens. A simple model is:

- generated unmarshalling performs schema-directed recovery and union
  selection;
- generated validation rejects invalid values that recovery does not cover;
- every outbound request, response, and notification is validated before it is
  written;
- every inbound params or result value is validated before it reaches user
  code.

Round-trip tests should distinguish canonical valid-value round trips from the
four open-union payloads that must retain raw extension data. Malformed fields
that intentionally default cannot also round-trip unchanged.

### 3. `Prompt` conflates request cancellation with turn cancellation

ACP defines two independent operations:

- `$/cancel_request` cooperatively cancels one JSON-RPC request;
- `session/cancel` cancels a prompt turn and requires the original
  `session/prompt` to finish with `StopReason` `cancelled`.

The proposed `Prompt` maps cancellation of its Go context to
`session/cancel`, then ignores the cancelled context and waits indefinitely for
the peer. That is surprising Go behavior and removes the caller's only bounded
way to stop waiting.

The TypeScript SDK maps its per-request cancellation signal to
`$/cancel_request`; it exposes `session/cancel` separately. The official MCP Go
SDK also returns `ctx.Err()` when a call context is cancelled and sends the
protocol cancellation notification on a separate, bounded context.

The Go API should keep the same separation:

- cancellation of the `Prompt` context sends `$/cancel_request` and lets
  `Prompt` return `ctx.Err()`;
- `ClientConn.Cancel(ctx, params)` sends `session/cancel`;
- a caller that wants to cancel a turn and await its final cancelled result
  keeps the `Prompt` context alive and calls `Cancel` separately;
- the connection read loop is independent of the `Prompt` waiter, so it keeps
  dispatching final `session/update` notifications and consuming the eventual
  response even if the original caller stopped waiting.

This preserves both Go context semantics and the ACP requirement to keep
accepting the tail of a cancelled turn.

### 4. Capability inference is internally contradictory

The proposed `ClientConfig` says that handlers imply capabilities, while a raw
`Capabilities` field may override the inference. It then claims that
advertising a capability without a handler is impossible. An unrestricted
override makes that mismatch expressible again.

Several ACP capabilities also do not map one-to-one to handler fields. In
particular, `terminal: true` advertises all five `terminal/*` methods. A
non-nil `CreateTerminal` handler cannot imply that the other four handlers
exist. Elicitation, authentication, session, and NES capabilities contain
similar nested distinctions.

Capability configuration should follow these rules:

- Group operations controlled by one aggregate capability. Terminal support,
  for example, is one all-or-nothing `TerminalHandlers` group containing all
  five handlers.
- Allow explicit capability configuration to refine a capability implemented
  by handlers, or to disable it, but never to enable an operation with no
  implementation.
- Validate configuration at construction time so every advertised capability
  has its complete handler set.
- Give required client behavior safe zero meanings where possible: a nil
  session-update handler may be a no-op, and a nil permission handler may
  return a cancelled or rejected outcome. Required agent operations that have
  no safe default should make construction fail explicitly.
- Reject inbound use of an unadvertised method, especially filesystem and
  terminal methods, because capabilities are an authority boundary rather than
  presentation metadata.
- Reject unsupported outbound calls locally from the negotiated peer
  capabilities instead of writing a request that cannot succeed.

The statement that every method after four baseline methods is
capability-gated should also be removed. `session/cancel` and `session/update`
are baseline session operations, among other exceptions.

### 5. The design omits the protocol's extension-method API

The v1 schema includes `ExtRequest`, `ExtResponse`, and `ExtNotification` in
both directions. The TypeScript SDK exposes generic request, notification, and
handler-registration operations for these extension messages.

The Go design hides all method strings and JSON-RPC envelopes, which is correct
for standard ACP operations, but it provides no deliberate escape hatch for
the extension mechanism. A caller could therefore not implement an ACP
extension without bypassing the SDK.

This does not require middleware or a builder framework. The API needs only a
narrow extension boundary:

- a raw or caller-decoded `Call` operation;
- a raw `Notify` operation;
- fallback request and notification handlers for methods not in the generated
  standard table;
- params and results represented by `json.RawMessage` or caller-supplied decode
  targets.

Standard ACP methods should remain strongly typed and should not expose their
method strings through normal use.

### 6. Generating all 265 definitions conflicts with hiding JSON-RPC

The schema's definitions include routing and envelope types such as
`AgentRequest`, `AgentResponse`, `ClientRequest`, `ClientResponse`, and
protocol-level error messages. Emitting all 265 definitions as exported types
in `acp` would expose the plumbing that the package-layout section explicitly
says is hidden, and would overlap the public `jsonrpc` package.

The generator still needs to understand every definition, but it must classify
output visibility:

- method params, results, notifications, and their reachable domain types are
  exported from `acp`;
- JSON-RPC envelopes and generated routing unions remain internal;
- only transport-facing message abstractions are exported from `jsonrpc`;
- method constants used only by generated dispatch stay unexported, while the
  extension API accepts an explicit method string.

This classification must be settled before the generated public API is
committed and checked by `gorelease`.

### 7. The transport signature is missing its behavioral contract

The proposed `Transport` and `Connection` signatures are suitable, but the
official MCP Go SDK's important requirements live in their comments, not their
method lists. The ACP contract must state at least that:

- a transport is connected at most once;
- `Write` may be called concurrently;
- `Read` may run concurrently with `Close`;
- `Close` is idempotent and concurrency-safe;
- `Close` unblocks a pending `Read`;
- a read or write failure closes the logical connection;
- each goroutine owned by the connection has a defined exit condition;
- `Wait` returns the connection's terminal error consistently.

`IOTransport` should accept closeable streams, not arbitrary
`io.Reader`/`io.Writer` values. Without a way to close the reader, a pending
read and its goroutine cannot be reliably stopped.

The connection initialization state also needs one owner. Prefer having
`Client.Connect` perform `initialize` and return an initialized `ClientConn`,
with an accessor for the initialization result. If `Initialize` remains a
public connection method instead, the design must define how calls before
initialization, repeated initialization, and concurrent initialization fail.

### 8. The v1 lane still contains experimental surface

The heading “Stable only” correctly rejects the separate schema-v2 alpha lane,
but it can be read as saying every v1 definition is stable. The v1.21.0 schema
itself marks providers, ACP-carried MCP, document synchronization, NES, session
forking, plan updates, and other definitions as unstable.

The design should call this “v1 schema lane only” and separately decide how
v1's unstable operations appear in Go documentation and release compatibility
checks. Generating their types in the first layer may expose public API before
the roadmap intends to implement the corresponding operations.

## Recommended roadmap changes

### Layer 1: wire-semantics spike

Prove the generator model with the representative cases listed above. Compare
accepted values, rejected values, default recovery, and serialized output
against the local TypeScript SDK. This is the cheapest point to discover that a
chosen Go representation cannot express the schema.

### Layer 2: complete wire layer

Generate the public payload types, internal routing types, method table,
validation, and codecs. Vendor both schema files. Make generation and
cross-SDK fixture comparison deterministic CI checks.

### Layer 3: link

Implement the transport concurrency and shutdown contracts, connection state
machine, request-level cancellation, and in-memory transport. Test shutdown and
cancellation without sleeps.

### Layer 4: turn

Implement session creation, prompts, updates, permission requests, and explicit
turn cancellation. Require interoperability with a TypeScript SDK process over
stdio before tagging `v0.1.0`; two endpoints from the same Go implementation can
share the same wire bug and are not sufficient release evidence.

### Layer 5: workspace and later capabilities

Add filesystem and the complete terminal handler group, then add later
capabilities only when the negotiated capability-to-handler invariant is
defined for each group.

## Resolution of the current open questions

- **Union decode helper:** do not add a generic `As[T]` initially. Direct type
  assertions and type switches are standard Go and already sufficient. Add a
  helper only if real callers show repeated logic that it materially improves.
- **Ungated methods:** refuse them. Reject unsupported outbound calls locally
  and reject inbound calls that violate the advertised contract. Keep extension
  methods on their separate explicit fallback path.
- **Signal handling in `Agent.Run`:** do not own operating-system signals in a
  library. `Run` should stop when its context is cancelled or its transport
  ends; a `main` package owns `signal.NotifyContext`.

## Advisory recommendations

- Pin reference commit hashes beside mutable source line references, as this
  review does. Line numbers in both SDKs move frequently.
- Preserve the upstream Go copyright and license notices when forking
  `jsonrpc2_v2`.
- Reconsider the Go 1.27 floor once implementation dependencies are known. The
  current public design uses `iter.Seq`, which requires only Go 1.23, and the
  official MCP Go SDK currently declares Go 1.25. A public library should not
  require a newer toolchain without a concrete language or dependency need.
