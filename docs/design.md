# The Go design

What the Agent Client Protocol looks like as a Go package, and why each decision
went the way it did. [protocol.md](./protocol.md) has the facts this is designed
against; sources are in [README.md](./README.md#sources).

The shape is taken from the official MCP Go SDK
(`~/Desktop/go-sdk`, <https://github.com/modelcontextprotocol/go-sdk>), which the
Go team wrote and argued for in its own `design/design.md`. That SDK solves the
same problem this one has — a bidirectional JSON-RPC protocol, capability-gated
optional methods, a large generated type set, and a spec that keeps moving — and
it solved it in Go rather than by transliterating an existing SDK. Where ACP
differs from MCP, this page says so and says what changed.

## Two numbers decide most of it

- **265 definitions** means the wire types are generated, not written. Nobody
  hand-maintains that against a moving upstream.
- **14 of 43 methods run agent → client**, so this is not a client library with a
  server mode bolted on. Both directions are the same machinery seen from
  opposite ends, and the package is built that way from the first commit.

## Package layout

```text
github.com/Tangerg/acp                        package acp — the whole user-facing API
github.com/Tangerg/acp/jsonrpc                message types, for custom transports only
github.com/Tangerg/acp/internal/jsonrpc2      the connection machinery
github.com/Tangerg/acp/internal/cmd/schemagen the generator
schema/schema.json                            the vendored upstream schema
```

One package for the API, as `net/http`, `net/rpc` and the MCP SDK all do
(`go-sdk/design/design.md:29`: "having a single package aids discoverability in
package documentation and in the IDE. Furthermore, it avoids arbitrary decisions
about package structure that may be rendered inaccurate by future evolution of
the spec").

The alternative — `acp/client`, `acp/agent`, `acp/types` — splits a symmetric
protocol down a seam that is not there. `RequestPermissionParams` is written by
the agent and read by the client; there is no honest package for it. A single
package also means the question never has to be reopened when the spec adds a
method that crosses the seam.

`jsonrpc` is public only because a custom transport has to name the message type
it carries. Nothing else about JSON-RPC appears in the API: not request IDs, not
the envelope, not the method strings. Those are plumbing, and a caller who has to
know them has been handed the plumbing.

## The wire types are generated

Upstream publishes `schema.json` and `meta.json` as assets on a release tag. The
TypeScript SDK downloads them, generates its types, and has a `--check` mode CI
runs to prove the committed output still matches
(`acp-typescript-sdk/scripts/generate.js:450` for the download,
`package.json`'s `generate:check` for the gate). Do the same:

- `schema/schema.json` is vendored and pinned to a release tag, currently
  `schema-v1.21.0`. The wire contract becomes a repository artifact that shows up
  in a diff when it moves.
- `internal/cmd/schemagen` reads it and emits the types.
- The output is committed, so `go get` needs no generator and pkg.go.dev has
  something to document.
- CI runs the generator and fails if the tree changes — the same promise as
  `go mod tidy -diff`.

Generated code stays inside the lint gate. `.golangci.yml` already disables
golangci-lint's implicit exclusion of generated files, because these are the
types every caller touches and so the ones most worth checking.

## Unions

Go has no sum type and the schema has 41 unions. An earlier draft of this page
split them into "14 open string enumerations and 27 discriminated struct unions"
and gave every struct union an unknown arm. Both halves of that were wrong, and
[design-review.md](./design-review.md#1-the-union-model-disagrees-with-the-v1-schema)
is why this section was rewritten. The correction matters because it inverts the
forward-compatibility rule: the schema, not this package, decides which unions
are open.

Classified by actual wire shape, the 41 are:

| Class | Count | Examples |
| --- | --- | --- |
| Closed string enumerations | 14 | `StopReason`, `ToolKind`, `ToolCallStatus`, `Role` |
| Open string unions — const arms plus a bare `string` | 3 | `LlmProtocol`, `CompactionStatus`, `SessionConfigOptionCategory` |
| Closed discriminated object unions | 16 | `SessionUpdate`, `ContentBlock`, `ToolCallContent` |
| Open object unions, with a schema `not` catch-all | 4 | `CreateElicitationRequest`, `CreateElicitationResponse`, `ElicitationPropertySchema`, `MultiSelectItems` |
| Primitive, value and mixed unions | 4 | `RequestId` (`string`\|integer\|`null`), `ErrorCode` (integer), `ElicitationContentValue`, `SessionConfigSelectOptions` |

The schema says which is which, explicitly, and the TypeScript generator already
honours it. `zStopReason` is five `z.literal` arms and nothing else;
`zLlmProtocol` is five literals **plus `z.string()`**
(`acp-typescript-sdk/src/schema/zod.gen.ts:2075` and `:1671`). Exactly four
unions carry the `not` catch-all that makes an object union open.

So:

```go
// StopReason is closed: the schema lists these five and no others.
type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled"
)

// LlmProtocol is open: the schema's last arm is a bare string, so a value
// outside this list is valid and is kept as received.
type LlmProtocol string
```

Decoding a value the schema does not permit is an error, not a value to keep.
Being more permissive than the published grammar is the local dialect
[AGENTS.md](../AGENTS.md) forbids — and it is not even safe permissiveness: a
`StopReason` this package invented would flow into caller `switch` statements
that the schema promised were exhaustive.

`SessionUpdate` is a **closed** 15-arm union in v1. It gets the sealed interface,
as the MCP SDK does for `Content` (`go-sdk/mcp/content.go:22`) —

```go
// A SessionUpdate is one of [UserMessageChunk], [AgentMessageChunk],
// [AgentThoughtChunk], [ToolCallStart], [ToolCallProgress], [PlanUpdate], …
type SessionUpdate interface {
	isSessionUpdate()
}
```

— and an unrecognised `sessionUpdate` discriminator is a decode error. The
`UnknownSessionUpdate` arm the earlier draft proposed is deleted: inventing a
catch-all the schema does not define is inventing wire behaviour.

The four genuinely open object unions get a raw arm, because there the schema
asks for one: the `not` clause defines what falls through, and the payload has to
survive intact.

The forward-compatibility worry that motivated the wrong design is real, but it
is upstream's to answer. If ACP wants `SessionUpdate` to tolerate a future arm,
that is a schema change — and the schema already shows it knows how to express
one.

## Validation is part of the codec, not an afterthought

`encoding/json` checks JSON syntax and Go field types. It does not implement this
schema. The v1 schema uses two extension keywords heavily —
**378 occurrences of `x-deserialize-default-on-error`** and **35 of
`x-deserialize-skip-invalid-items`** — and neither has any meaning to a plain
decoder. `ClientCapabilities.fs` carries both a default and
`x-deserialize-default-on-error`, so a malformed `fs` object must become
`{readTextFile:false, writeTextFile:false}` rather than fail the message.

Also unenforced by plain decoding: required fields, constants, enum membership,
numeric bounds, lengths, formats, the difference between omitted / `null` /
present where it matters, required slices (a nil Go slice encodes as `null`,
which is invalid where an array is required), and the `not` rules of the four
open unions.

So the codec has a stated shape:

- Generated unmarshalling performs schema-directed recovery and union selection.
- Generated validation rejects what recovery does not cover.
- Every outbound request, response and notification is validated before it is
  written.
- Every inbound params or result is validated before it reaches user code.

Round-trip tests split in two, because these are different promises: canonical
valid values round-trip byte-identically, and the four open unions preserve their
extension payload. A field that intentionally defaults on malformed input cannot
also round-trip unchanged, and a test that asks for both will be wrong about one.

## The API

One place. Everything below this section explains a decision recorded here and
does not restate a signature — the previous revision left three mutually
exclusive sketches in three sections, which
[design-review-2.md](./design-review-2.md#1-the-documents-still-specify-three-incompatible-connection-apis)
caught. If prose and this table disagree, this table is wrong and must be fixed
here rather than contradicted there.

```go
// Construction. Returns an error because the capability invariant is checked
// here: a configuration whose advertisement exceeds its implementation is
// rejected before it can accept a request it cannot serve.
func NewClient(*ClientConfig) (*Client, error)
func NewAgent(*AgentConfig) (*Agent, error)

// Connecting performs initialize; there is no public Initialize.
func (*Client) Connect(context.Context, Transport) (*ClientConn, error)
func (*Client) Conns() iter.Seq[*ClientConn]
func (*Agent) Connect(context.Context, Transport) (*AgentConn, error)
func (*Agent) Run(context.Context, Transport) error

func (*ClientConn) Initialized() *InitializeResult
func (*ClientConn) Authenticate(context.Context, *AuthenticateParams) (*AuthenticateResult, error)
func (*ClientConn) NewSession(context.Context, *NewSessionParams) (*ClientSession, *NewSessionResult, error)
func (*ClientConn) LoadSession(context.Context, *LoadSessionParams) (*ClientSession, *LoadSessionResult, error)
func (*ClientConn) Call(context.Context, string, any, any) error   // extension methods only
func (*ClientConn) Notify(context.Context, string, any) error      // extension methods only
func (*ClientConn) Close() error
func (*ClientConn) Wait() error

// The conversation, as the client sees it. Binds SessionID and nothing else.
func (*ClientSession) ID() SessionID
func (*ClientSession) Prompt(context.Context, *PromptParams) (*PromptResult, error)
func (*ClientSession) Cancel(context.Context, *CancelParams) error
func (*ClientSession) SetMode(context.Context, *SetModeParams) (*SetModeResult, error)

// The same conversation as the agent sees it: a different set of operations,
// so a different type.
func (*AgentSession) ID() SessionID
func (*AgentSession) Update(context.Context, *SessionUpdateParams) error
func (*AgentSession) RequestPermission(context.Context, *RequestPermissionParams) (*RequestPermissionResult, error)
func (*AgentSession) ReadTextFile(context.Context, *ReadTextFileParams) (*ReadTextFileResult, error)
func (*AgentSession) WriteTextFile(context.Context, *WriteTextFileParams) (*WriteTextFileResult, error)
func (*AgentSession) CreateTerminal(context.Context, *CreateTerminalParams) (*Terminal, *CreateTerminalResult, error)

func (*Terminal) ID() TerminalID
func (*Terminal) Output(context.Context, *TerminalOutputParams) (*TerminalOutputResult, error)
func (*Terminal) WaitForExit(context.Context, *WaitForExitParams) (*WaitForExitResult, error)
func (*Terminal) Kill(context.Context, *KillTerminalParams) error
func (*Terminal) Release(context.Context, *ReleaseTerminalParams) error
```

Every operation takes `(ctx, *Params)` and returns `(*Result, error)`, with no
convenience overloads, because when the spec adds an optional field a params
struct absorbs it and a positional signature cannot
(`go-sdk/design/design.md:371`). `nil` params stays valid wherever none are
required today.

`ClientSession` and `AgentSession` are separate types rather than one `Session`
with both method sets. A client never calls `RequestPermission`; an agent never
calls `Prompt`. One type carrying both would make those calls compile and fail at
runtime, which is a worse place to find out.

## Why connections and sessions are different types

The MCP SDK separates `Client` from `ClientSession`: the first holds handlers and
features, the second is one logical connection, and one client may hold many
(`go-sdk/design/design.md:279`). That split is right and it carries over. Its
name does not.

**ACP already means something by "session."** `session/new` returns a
`sessionId`; a session is a conversation with its own history, and one connection
carries many. Calling the connection a session too would put
`ClientSession.NewSession` in the API. So the connection is `ClientConn` and the
conversation is `ClientSession`.

The conversation earns a type because **32 schema definitions carry a `sessionId`
property** — request types, but also `NewSessionResponse`, `ForkSessionResponse`,
`SessionInfo`, `ElicitationSessionScope` and several notifications. Threading
that string by hand across the operations that need it is the same mistake as
threading a terminal ID through five functions.

Handles are **connection-bound**. A `SessionID` outlives the connection it came
from, but a handle does not: `LoadSession` and `ResumeSession` on a new
connection return a new handle. A handle that silently re-pointed at a different
transport would be a lifetime nobody can reason about, so callers keep the
`SessionID` and ask for a fresh handle. This closes the open question the
previous revision left.

## Handles bind identifiers and nothing else

The risk with a handle is that it becomes a lossy summary of the wire. Two
mechanical rules stop that.

**On the way out: the API params type is the generated wire type minus the
identifiers the handle owns.** `PromptParams` is `PromptRequest` without
`sessionId`; `TerminalOutputParams` is `TerminalOutputRequest` without
`sessionId` and `terminalId`. Both are generated, so every other field — including
`_meta` — survives, and the ID exists exactly once and cannot disagree with
itself. This is why `Cancel` and `Release` still take params rather than only a
context: they carry `_meta` too.

The generated wire type remains the codec type. The projection is an API surface
over it, not a replacement for it.

**On the way back: the response is returned, not absorbed.** `NewSessionResponse`
carries `modes`, `configOptions` and `_meta` besides `sessionId`, so returning
only a handle would make three fields unreachable — the TypeScript SDK keeps the
whole response on its `ActiveSession` (`src/acp.ts:768`). Hence
`(*ClientSession, *NewSessionResult, error)`. Three results is mildly ugly and
strictly lossless, and the alternative of hanging typed accessors off the handle
does not survive `LoadSession` and `ResumeSession` returning different result
types.

## Errors

`ErrorCode` is **not** eight values. The schema lists eight predefined constants
and then a ninth arm titled *Other* — an unrestricted `int32` — and the
TypeScript type is correspondingly the eight literals plus `number`
(`src/schema/types.gen.ts:3605`). An unknown in-range code is valid and must
survive.

```go
// ErrorCode is int32 because the schema's open arm is int32-formatted; a wider
// Go type would admit codes that cannot be encoded.
type ErrorCode int32

const (
	CodeParseError       ErrorCode = -32700
	CodeInvalidRequest   ErrorCode = -32600
	CodeMethodNotFound   ErrorCode = -32601
	CodeInvalidParams    ErrorCode = -32602
	CodeInternalError    ErrorCode = -32603
	CodeRequestCancelled ErrorCode = -32800
	CodeAuthRequired     ErrorCode = -32000
	CodeResourceNotFound ErrorCode = -32002
)

// An Error is a JSON-RPC error returned by the peer.
type Error struct {
	Code    ErrorCode
	Message string
	Data    json.RawMessage
}

func (e *Error) Error() string
// Is compares codes, so errors.Is(err, ErrAuthRequired) works against any error
// carrying that code. Wrapping alone would only have made errors.As work.
func (e *Error) Is(target error) bool
```

`ErrAuthRequired` and its siblings are `*Error` values carrying only a code;
`(*Error).Is` is what makes them usable as sentinels. `-32000` is the one that
matters, because it is not a failure — it is how an agent answers `session/new`
to say "authenticate first", a documented step in the lifecycle:

```go
sess, res, err := conn.NewSession(ctx, params)
if errors.Is(err, acp.ErrAuthRequired) {
	// Expected. Authenticate, then retry.
}
```

**`-32800` is not unconditionally `context.Canceled`.** The schema says it may
arise from caller cancellation *"or because of resource constraints or
shutdown"*, so a remote `-32800` does not prove the local context was cancelled.
The previous revision mapped it unconditionally and was wrong. The rule:

1. If the call's context is done, return `ctx.Err()` — preserving
   `context.DeadlineExceeded` rather than flattening every timeout to
   `context.Canceled`.
2. If the context is live and the peer answers `-32800`, keep the `*Error`. It
   matches `ErrRequestCancelled` by code, and it means the peer gave up, which is
   a different fact.

Outbound, a handler that returns a plain `error` sends a stable *Internal error*
and logs the detail locally. Handler errors routinely carry paths, hostnames and
internal identifiers, and the peer is not entitled to them. A handler that
returns an `*Error` chooses the code, message and data deliberately. Validation
failures caught before dispatch are `-32602`; an unimplemented or unadvertised
method is `-32601`, which is the concrete form of refusing an ungated call.

## Two cancellations, and who owns each

`$/cancel_request` cancels one JSON-RPC request. `session/cancel` cancels a turn
and obliges the agent to finish the outstanding `session/prompt` with
`StopReason` `cancelled`. An earlier draft fused them, mapping `Prompt`'s context
onto `session/cancel` and then ignoring the cancelled context — which took away
the caller's only bounded way to stop waiting. Both references do the opposite
(`acp-typescript-sdk/src/jsonrpc.ts:99`, `go-sdk/mcp/transport.go:281`).

So cancelling a `Prompt` context sends `$/cancel_request` and returns
`ctx.Err()`, and `ClientSession.Cancel` sends `session/cancel`. Separating them
fixed the public meaning but left the obligations unowned, and they cannot be an
application's problem:

**The connection owns pending permission requests, indexed by session.** When
`session/cancel` is sent or received, every pending
`session/request_permission` for that session must be answered with the
`cancelled` outcome — while the user's handler may still be blocked on a dialog.
The connection cancels those handler contexts, resolves each response exactly
once, and wins the race against a late user decision. `Session.Cancel` returns
before any of that has finished, so it cannot be the caller's job.

**The agent side owns one context per turn.** Receiving `session/cancel` cancels
the turn's context and nothing else on the connection; other sessions and
unrelated calls are untouched. Receiving `$/cancel_request` cancels that request
and work nested under it, but makes no promise about the `session/cancel` result
shape. The context tree is: connection → session → turn → request, and each level
is cancelled only by its own signal.

There is **no safe nil permission handler**. The wire outcome is either
`cancelled` or a selected option ID, and an agent is not obliged to offer a
reject option, so there is no universal "deny" to synthesise. Since
`session/request_permission` is baseline client behaviour, a missing handler
fails construction. The previous revision's "a nil permission handler denies" was
inventing an outcome the protocol does not define.

## Handlers, capability groups, and a checked table

The TypeScript SDK declares `interface Client` with fourteen methods, twelve
optional (`src/acp.ts:3723`). Go has no optional interface methods, and the
transliteration makes every caller write stubs and keeps capability advertisement
as a second list to maintain by hand. So handlers are fields on a `Config`,
following `go-sdk/mcp/client.go`.

The unit is the **capability group**, not the method, because ACP's capabilities
are not one-to-one with methods. `ClientCapabilities.terminal` is one boolean
documented as "Whether the Client support all `terminal*` methods"; `fs` is two
independent booleans:

```go
type ClientConfig struct {
	// Identifies this client during initialize, which Connect performs.
	Info *Implementation
	Meta map[string]any

	// A notification has no response, so its handler returns nothing. A request
	// handler returns a result and an error, which becomes the peer's error.
	SessionUpdate     func(context.Context, *SessionUpdateRequest)
	RequestPermission func(context.Context, *RequestPermissionRequest) (*RequestPermissionResult, error)

	ReadTextFile  func(context.Context, *ReadTextFileRequest) (*ReadTextFileResult, error)
	WriteTextFile func(context.Context, *WriteTextFileRequest) (*WriteTextFileResult, error)
	Terminal      *TerminalHandlers // all five, or none

	// nil: advertise exactly what the handlers above support.
	// non-nil: the complete desired advertisement, not a patch. Construction
	// fails if it asks for anything the handlers do not implement.
	Capabilities *ClientCapabilities
}
```

`Capabilities` is a **complete replacement, never a merge**. A field-by-field
merge would need to distinguish "set to false" from "not set", and the generated
capability type has no third state for a scalar boolean to express that. So nil
means derive, non-nil means this-exactly, and either way construction rejects an
advertisement the implementation cannot back. That is also why `NewClient`
returns an error.

`Connect` needs `InitializeParams` from somewhere, and inventing them is not an
option: the schema wants a protocol version, and allows client information and
`_meta`. The version is the package's (`ProtocolVersion`); `Info` and `Meta` come
from the config above.

### The capability table is hand-maintained and checked

"Whatever the schema's capability types say" is not implementable. The schema
annotates method payloads with `x-method` and `x-side` — 74 occurrences each —
and has **no annotation linking a method to a capability**. Those links exist
only in prose descriptions, and some capabilities describe accepted data rather
than an optional method.

So the gate is a version-pinned table in this repository recording, per standard
method: baseline or gated, the direction the check applies in, the exact
predicate, and the complete handler group required to advertise it. CI verifies
it covers every method in `meta.json` and names no method that has been removed,
so a schema bump that adds or drops a method fails loudly rather than silently
leaving a hole in the gate. Each non-trivial predicate gets a test.

## Extension methods, with standard names reserved

The v1 schema defines `ExtRequest`, `ExtResponse` and `ExtNotification` in both
directions. Without an escape hatch an ACP extension cannot be implemented
through this package at all, so `Call` and `Notify` exist — but they take an
arbitrary string, and an unrestricted string is a hole straight through every
invariant above. A caller could pass `session/prompt` and bypass the generated
params type, outbound validation, session-ID binding and the capability gate; a
fallback handler could intercept a standard method that was merely misspelled.

**The generated standard-method set is reserved.** `Call` and `Notify` reject
those names, and fallback handlers run only for names outside the set. A standard
method has exactly one path through the typed codec and the capability gate. If a
diagnostic tool ever needs raw access to a standard method, that is a separately
named unsafe API, not a side effect of ordinary extension support.

## What is generated, and what is exported

Generating all 265 definitions as exported types would contradict the package
layout: `AgentRequest`, `ClientResponse` and the envelope types are JSON-RPC
plumbing, and exporting them from `acp` would publish what the layout says is
hidden and overlap the `jsonrpc` package. So the generator classifies output:

- Method params, results, notifications and every domain type they reach are
  exported from `acp`.
- Envelopes and generated routing unions stay unexported.
- Only the transport-facing message abstraction is exported from `jsonrpc`.
- Method-name constants used by generated dispatch stay unexported; the extension
  API takes an explicit string, checked against the reserved set.

### A root manifest, not a number in three documents

Scope is the transitive `$ref` closure of what is implemented. The previous
revision wrote that closure's size — 156 — into three documents, and the number
was already stale: it was computed from a root list that included `authenticate`,
which the roadmap did not implement. A count maintained by hand in prose is a
second source of truth, and it drifted within one revision.

So the roots are a committed manifest, naming separately:

- implemented request, response and notification payloads;
- protocol plumbing those operations need — `Error` and `ErrorCode` are reached
  from response envelopes rather than from any method's params, so they are roots
  in their own right the moment `Error` is exported;
- marker types the extension boundary requires.

CI computes the closure from the manifest and fails if the exported set differs.
The count becomes an output to check rather than a fact to maintain, and adding
an operation shows up as a diff in exactly one file.

## Transports

The MCP SDK's interface, unchanged (`go-sdk/mcp/transport.go:52`), because it is
the minimum a bidirectional JSON-RPC link needs and the easiest thing for a
caller to implement:

```go
type Transport interface {
	Connect(context.Context) (Connection, error)
}

type Connection interface {
	Read(context.Context) (jsonrpc.Message, error)
	Write(context.Context, jsonrpc.Message) error
	Close() error
}
```

- `CommandTransport` — the client side: start the agent and frame ndjson over its
  stdin and stdout.
- `StdioTransport` — the agent side.
- `IOTransport` — a closeable stream pair, not bare `io.Reader`/`io.Writer`:
  without a way to close the reader, a pending `Read` and the goroutine blocked
  in it cannot be stopped.
- `InMemoryTransport` — a client and an agent in one test with no subprocess. The
  reason the rest of the package never touches `os`.

The signatures are the easy half. In the MCP SDK the requirements that matter
live in the comments, and this package has to state its own, because they are
what a custom transport is being asked to promise:

- A transport is connected at most once.
- `Write` may be called concurrently.
- `Read` may run concurrently with `Close`.
- `Close` is idempotent and safe to call concurrently, and it unblocks a pending
  `Read`.
- A failed read or write closes the logical connection.
- Every goroutine the connection owns has a defined exit condition.
- `Wait` returns the connection's terminal error, consistently, to every caller.

Untested concurrency contracts are not contracts, so these are tested with
`testing/synctest` rather than with sleeps.

`Client.Connect` performs `initialize` and returns a connection that is already
initialized. A public `Initialize` would make three failure modes the API has to
define and test — a call before initialization, a second one, and two concurrent
ones — and doing it in `Connect` makes all three unrepresentable, which is
cheaper than defining them.

**An agent's stdout is the protocol stream.** One `fmt.Println` corrupts it, and
the failure surfaces as an unrelated parse error at the other end.
`StdioTransport` names the streams it uses rather than reaching for the globals,
so the collision is visible in the code.

### JSON-RPC

Fork `golang.org/x/tools/internal/jsonrpc2_v2` into `internal/jsonrpc2`, which is
what the MCP SDK did (`go-sdk/design/design.md:45`: "The Go team has a
battle-tested JSON-RPC implementation that we use for gopls, our Go LSP server"),
with upstream's copyright and licence headers intact. It is already
bidirectional and already handles request lifetime, async calls and cancellation
— the three things a hand-rolled version gets wrong first. It is `internal`
because it is an implementation detail, and a fork because the package is
`internal` upstream and cannot be imported.

ACP's request cancellation is `$/cancel_request`, LSP's spelling rather than
MCP's `notifications/cancelled`, so the fork's cancellation wiring is closer to
what this needs than it was for MCP.

## The v1 schema lane only, which is not the same as stable

Upstream ships v1 (`schema.json`, tag `schema-v1.21.0`) and an unstable v2
(`schema/v2/schema.unstable.json`, tag `schema-v2.0.0-alpha.3`), and the
TypeScript SDK carries both. This module implements the v1 lane and nothing else
until v2 stabilises. Two type sets in one package would double the generated
surface and grow every union arms that exist only in a draft.

That is a statement about lanes, not about stability. **52 of the v1 schema's 265
definitions carry an UNSTABLE marker** — "This capability is not part of the spec
yet, and may be removed or changed at any point."

The obvious response is to defer generating the unstable ones. Counting says that
is not available:

- **38** definitions are marked unstable *as types*. Those could be deferred.
- **14** are stable types with unstable *fields* — and they are the central ones:
  `AgentCapabilities`, `ClientCapabilities`, `SessionUpdate`, `PromptResponse`,
  `ToolCall`, `ToolCallUpdate`. Nothing can be deferred here.
- Following every `$ref` from the operations layers 3 to 5 implement, **18 of the
  38 type-level-unstable types are inside the closure**. `SessionUpdate` has plan
  and compaction arms; `NewSessionRequest` takes MCP servers. Only 20 are out of
  reach.

So instability cannot be the axis, and two rules replace it. **A generated type
is generated whole** — every field, every union arm. Upstream's marker warns that
a shape may change; it is not permission to omit it, and dropping an unstable arm
of `SessionUpdate` would fail to decode a message an agent may legitimately send.
**The compatibility promise carves them out explicitly, before v1.0**: symbols
whose schema definition carries the UNSTABLE marker are exempt from the module's
compatibility guarantee, their doc comments repeat upstream's warning verbatim,
and `gorelease` findings against them are reviewed rather than automatically
blocking. Pre-1.0 this costs nothing, which is why it is written down now instead
of discovered at the v1.0 tag.

## What is deliberately not copied from the TypeScript SDK

- `AgentApp`/`ClientApp` and the `connectWith` builder (`src/acp.ts:1841`,
  `:2093`). That is a framework on top of the protocol; the Go version is a
  `Config` struct and a `Connect` call.
- Zod schemas alongside types (`src/schema/zod.gen.ts`, 4,246 lines). Go does not
  need a second validator library — but it does need the validation, and that is
  **generated** from the same schema constraints, over a small shared runtime.
  Hand-written checks are reserved for invariants the schema cannot express, such
  as the reserved-method-name rule. An earlier draft said "a hand-written check",
  which contradicted the codec section; at this schema size, hand-written
  validation is not a plan.
- The 4,252-line `acp.ts`. Most of its size is the two type sets and the handler
  routing tables that generation should be producing.

Middleware, which the MCP SDK exposes on both sides
(`go-sdk/design/design.md:405`), is also not in the first version. Dispatch is
routed through one internal handler type from the start so it can be added later
without an API break, but an extension point with nothing behind it is the
speculative abstraction [AGENTS.md](../AGENTS.md) rules out.
