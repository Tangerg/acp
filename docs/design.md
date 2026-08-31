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

## Client and agent

The MCP SDK separates `Client` from `ClientSession`: the first holds handlers and
features, the second is one logical connection, and one client may hold many
(`go-sdk/design/design.md:279`). That split is right and it carries over. Its
name does not.

**ACP already means something by "session."** `session/new` returns a
`sessionId`; a session is a conversation with its own history, and one connection
carries many. Calling the connection a session too would put
`ClientSession.NewSession` in the API, and nobody should have to work out which
"session" that sentence means. So:

- **Connection** — the JSON-RPC link. `ClientConn`, `AgentConn`.
- **Session** — the ACP conversation, addressed by `SessionID`. Never a Go type
  that owns a socket.

```go
func NewClient(c *ClientConfig) *Client
func (*Client) Connect(ctx context.Context, t Transport) (*ClientConn, error)
func (*Client) Conns() iter.Seq[*ClientConn]

func (*ClientConn) Initialize(context.Context, *InitializeParams) (*InitializeResult, error)
func (*ClientConn) NewSession(context.Context, *NewSessionParams) (*NewSessionResult, error)
func (*ClientConn) Prompt(context.Context, *PromptParams) (*PromptResult, error)
func (*ClientConn) Cancel(context.Context, *CancelParams) error
func (*ClientConn) Close() error
func (*ClientConn) Wait() error
```

Symmetrically `NewAgent`, `Agent.Connect`, `AgentConn`, plus `Agent.Run` for the
case that is nearly all of them — serve one connection on stdio until the client
goes away.

Every spec method takes a context and a params pointer and returns a result
pointer and an error, with no convenience overloads. This is the MCP SDK's rule
(`go-sdk/design/design.md:371`) and the reasoning transfers exactly: when the
spec adds an optional field to a request, a params struct absorbs it and a
positional signature cannot. Passing `nil` params stays valid for any method that
currently needs none.

## Sessions are a handle, for the same reason terminals are

The section below argues that `terminal/*` should not be five functions threading
an ID string. **32 of the schema's request types carry a `sessionId`** — from
`PromptRequest` and `SessionNotification` to `ReadTextFileRequest` and every
`terminal/*` request — so the argument applies far more strongly to sessions, and
an earlier draft of this page made it for terminals and then left sessions as a
bare string. That was inconsistent.

```go
func (*ClientConn) NewSession(context.Context, *NewSessionParams) (*Session, error)
func (*ClientConn) LoadSession(context.Context, *LoadSessionParams) (*Session, error)

func (*Session) ID() SessionID
func (*Session) Prompt(context.Context, *PromptParams) (*PromptResult, error)
func (*Session) Cancel(context.Context) error
func (*Session) SetMode(context.Context, *SetModeParams) error
func (*Session) Close(context.Context) error
```

A `*Session` holds its connection and its ID and nothing else; it is not a second
place where connection state lives. It exists so that the ID is threaded once, at
the point the protocol says it comes from, instead of at 32 call sites where it
can be threaded wrong.

This does not weaken the naming rule from the previous section — it is what makes
it pay. `Session` is the conversation, `ClientConn` is the link, and now both are
Go types whose methods are exactly the operations the protocol scopes to them.
`ClientConn.Prompt` does not exist; `Session.Prompt` does, because a prompt
without a session is not a thing the protocol can express.

On the agent side the same handle carries the callbacks that are scoped to a
session: `session.RequestPermission(...)`, `session.ReadTextFile(...)`,
`session.CreateTerminal(...)`.

## Errors

The design said nothing about errors until this revision, which left the
`error` returned by every method undefined. ACP names eight codes:

| Code | Meaning |
| --- | --- |
| `-32700` | Parse error |
| `-32600` | Invalid request |
| `-32601` | Method not found |
| `-32602` | Invalid params |
| `-32603` | Internal error |
| `-32800` | Request cancelled |
| `-32000` | Authentication required |
| `-32002` | Resource not found |

```go
// An Error is a JSON-RPC error returned by the peer.
type Error struct {
	Code    int64
	Message string
	Data    json.RawMessage
}

func (e *Error) Error() string
```

Returned wrapped, so `errors.As` reaches it and the calling site reads normally.

Two of these are not failures and must not be handled as though they were.
**`-32000` authentication required** is how an agent answers `session/new` to say
"call `authenticate` first"; it is a step in the documented lifecycle, and a
caller has to be able to branch on it without string-matching a message:

```go
sess, err := conn.NewSession(ctx, params)
if errors.Is(err, acp.ErrAuthRequired) {
	// Expected: authenticate, then retry. Not a failure.
}
```

**`-32800` request cancelled** is what a peer returns for a request killed by
`$/cancel_request`, so it is the wire form of the caller's own `ctx` being
cancelled. It maps to `context.Canceled` rather than surfacing as a protocol
error, because a caller that cancelled its own context should not have to know
which of the two layers noticed first.

In the other direction, a handler that returns a plain `error` becomes
`-32603 Internal error` with its message; a handler that returns an `*Error`
controls the code. Validation failures found before dispatch are `-32602`, and an
unimplemented or unadvertised method is `-32601` — which is the concrete form of
the "refuse an ungated method" rule.

## Terminals are a handle

`terminal/create` returns an ID that four more methods take. Five free functions
threading a string is the wire shape, not an API.

```go
func (*AgentConn) CreateTerminal(context.Context, *CreateTerminalParams) (*Terminal, error)

func (*Terminal) ID() string
func (*Terminal) Output(context.Context) (*TerminalOutput, error)
func (*Terminal) WaitForExit(context.Context) (*TerminalExitStatus, error)
func (*Terminal) Kill(context.Context) error
func (*Terminal) Release(context.Context) error
```

The TypeScript SDK reached the same conclusion with `TerminalHandle`
(`acp-typescript-sdk/src/acp.ts:2962`). The handle also gives the release
obligation somewhere to live: a terminal that is never released leaks a process
on the client, and a type with a `Release` method is where `defer` goes.

## Two cancellations, kept apart

ACP has two independent operations and an earlier draft of this page fused them.
`$/cancel_request` cancels one JSON-RPC request. `session/cancel` cancels a
*turn*, and obliges the agent to finish the outstanding `session/prompt` with
`StopReason` `cancelled`.

That draft mapped `Prompt`'s context cancellation onto `session/cancel` and then
ignored the cancelled context and waited for the peer. It was wrong twice:
`ctx` is the caller's only bounded way to stop waiting, and taking it away leaves
no way to give up on an unresponsive agent short of dropping the connection. Both
reference implementations do the opposite —
`acp-typescript-sdk/src/jsonrpc.ts:99` ("Aborting this signal sends
`$/cancel_request` for the outgoing request"), and the MCP Go SDK returns
`ctx.Err()` and sends the protocol notification on a separate
`context.WithoutCancel` with its own timeout so a slow peer cannot hold the
caller past its deadline (`go-sdk/mcp/transport.go:281`).

So the two stay separate:

```go
// Prompt runs one turn. Cancelling ctx cancels the request: Prompt sends
// $/cancel_request and returns ctx.Err(), like any other Go call that does I/O.
//
// It does not cancel the turn. To do that, call Cancel — see the note below on
// why the two are not the same thing.
func (*ClientConn) Prompt(context.Context, *PromptParams) (*PromptResult, error)

// Cancel ends the current turn. The agent answers the outstanding Prompt with
// StopReason "cancelled" after it has finished reporting; a caller that wants
// that final result keeps its Prompt context alive and waits.
func (*ClientConn) Cancel(context.Context, *CancelParams) error
```

The protocol's requirement — that the tail of a cancelled turn is still
delivered — is met by where the read loop lives, not by making `Prompt` block.
The connection dispatches inbound messages independently of whoever is waiting on
a call, so `session/update` notifications keep reaching handlers and the eventual
response is still consumed even if the original caller has walked away. That is
the property to test: cancel a turn, drop the waiter, and assert the updates
still arrive.

## Handlers are fields, and capabilities are derived from complete groups

The TypeScript SDK declares `interface Client` with fourteen methods, twelve
optional (`acp-typescript-sdk/src/acp.ts:3723`). Go has no optional interface
methods, and the transliteration — fourteen methods where twelve may return
"unsupported" — makes every caller write stubs and makes capability advertisement
a second list to keep in step by hand.

So handlers are fields on a `Config`, following `go-sdk/mcp/client.go`'s
`ClientOptions`. But the unit is the **capability group**, not the individual
handler, because ACP's capabilities are not one-to-one with methods:

```go
type ClientConfig struct {
	// A notification has no response, so its handler returns nothing. There is
	// nowhere for an error to go but a log, and a signature that pretends
	// otherwise invites a caller to believe the peer will hear about it.
	SessionUpdate func(context.Context, *SessionUpdateRequest)

	// A request handler returns a result and an error, which becomes the
	// JSON-RPC error the peer sees.
	RequestPermission func(context.Context, *RequestPermissionRequest) (*RequestPermissionResult, error)

	// fs.readTextFile and fs.writeTextFile are separate booleans in the schema,
	// so these are separate fields.
	ReadTextFile  func(context.Context, *ReadTextFileRequest) (*ReadTextFileResult, error)
	WriteTextFile func(context.Context, *WriteTextFileRequest) (*WriteTextFileResult, error)

	// ClientCapabilities.terminal is one boolean meaning "supports all
	// terminal* methods", so the five arrive together or not at all.
	Terminal *TerminalHandlers

	// Capabilities may refine or disable what the handlers imply. It may not
	// enable an operation that has no implementation.
	Capabilities *ClientCapabilities
}
```

The split in that struct is the whole convention: notifications return nothing,
requests return `(result, error)`. It follows `go-sdk/mcp/client.go`, whose
notification handlers are `func(context.Context, *T)` with no error, and it means
the signature alone says which kind of message a handler is for.

`terminal` is the case that kills per-handler inference: the schema field is a
plain boolean documented as "Whether the Client support all `terminal*` methods".
A non-nil `CreateTerminal` cannot imply that `Output`, `WaitForExit`, `Kill` and
`Release` exist, so a group is the only honest unit. `fs` genuinely is two
booleans, so those stay separate. Elicitation, auth, session and NES have their
own nesting and each gets whatever grouping its capability type actually has.

Three rules make the invariant hold rather than merely be claimed:

- **Construction validates.** Every advertised capability must have its complete
  handler set, checked when the `Client` or `Agent` is built, not when a request
  arrives.
- **The override may narrow, never widen.** An earlier draft said advertising a
  capability without a handler was inexpressible and then supplied a
  `Capabilities` field that made it expressible again. `Capabilities` can refine
  or switch a capability off; enabling one with nothing behind it is a
  construction error.
- **Capabilities are an authority boundary.** They decide whether an agent may
  read a file or run a command, so an inbound call to an unadvertised method is
  refused, and an outbound call the peer never advertised fails locally rather
  than going out to be rejected.

Required behaviour gets safe zero meanings where one exists: a nil
`SessionUpdate` handler is a no-op, a nil permission handler denies. Where no
safe default exists, construction fails and says so.

This is a `Config` struct with useful zero values rather than functional options,
per [AGENTS.md](../AGENTS.md).

## Extension methods

The v1 schema defines `ExtRequest`, `ExtResponse` and `ExtNotification` in both
directions, and the TypeScript SDK exposes generic call, notify and handler
registration for them. Hiding every method string and JSON-RPC envelope is right
for the standard operations and wrong here: without an escape hatch, an ACP
extension cannot be implemented through this package at all.

The boundary is narrow and deliberately unglamorous — no middleware, no builder:

```go
func (*ClientConn) Call(ctx context.Context, method string, params, result any) error
func (*ClientConn) Notify(ctx context.Context, method string, params any) error
```

plus fallback request and notification handlers on the `Config` for methods
outside the generated table. Params and results are `json.RawMessage` or a
caller-supplied decode target. Standard methods stay strongly typed and never
expose their method strings through ordinary use.

## What is generated, and what is exported

Generating all 265 definitions as exported types would contradict the package
layout above: `AgentRequest`, `ClientResponse` and the protocol error types are
JSON-RPC routing and envelope plumbing, and exporting them from `acp` would both
publish the plumbing and overlap the `jsonrpc` package.

The generator reads every definition and classifies where the output goes:

- Method params, results, notifications and every domain type they reach are
  exported from `acp`.
- Envelopes and generated routing unions stay unexported.
- Only the transport-facing message abstraction is exported from `jsonrpc`.
- Method-name constants used by generated dispatch stay unexported; the
  extension API takes an explicit string instead.

This has to be settled before the first generated API is committed, because from
that point `gorelease` treats it as a promise.

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

### Who owns initialization

`initialize` must happen once, before anything else, and its result is
connection-wide state. `Client.Connect` therefore performs it and returns a
`ClientConn` that is already initialized, with an accessor for the result.

The alternative — a public `Initialize` method — makes three failure modes the
API has to define and test: a call before initialization, a second
initialization, and two concurrent ones. Doing it in `Connect` makes all three
unrepresentable, which is cheaper than defining them.

HTTP and WebSocket are work in progress upstream and are not implemented until
they settle.

**An agent's stdout is the protocol stream.** One `fmt.Println` corrupts it, and
the failure surfaces as an unrelated parse error at the other end.
`StdioTransport` therefore names the streams it uses rather than reaching for the
globals, so the collision is visible in the code; the package documents the
hazard and points logging at stderr.

### JSON-RPC

Fork `golang.org/x/tools/internal/jsonrpc2_v2` into `internal/jsonrpc2`, which is
what the MCP SDK did (`go-sdk/design/design.md:45`: "The Go team has a
battle-tested JSON-RPC implementation that we use for gopls, our Go LSP server").
It is already bidirectional and already handles request lifetime, async calls and
cancellation — the three things a hand-rolled version gets wrong first. It is
`internal` because it is an implementation detail, and it is a fork because the
package is `internal` upstream and cannot be imported.

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
yet, and may be removed or changed at any point." An earlier heading here read
"Stable only" and invited exactly the wrong inference.

The obvious response is to defer generating the unstable ones. Counting says
that is not available, and the counting is worth showing because it changes the
answer:

- **38** definitions are marked unstable *as types*. Those could be deferred.
- **14** are stable types with unstable *fields* — and they are the central ones:
  `AgentCapabilities`, `ClientCapabilities`, `SessionUpdate`, `PromptResponse`,
  `ToolCall`, `ToolCallUpdate`. Nothing can be deferred here; the type is needed
  in layer 3.
- Taking the 26 method types that layers 3 to 5 implement and following every
  `$ref`, the reachable closure is **156 of the 265** definitions — and **18 of
  the 38 type-level-unstable types are inside it**. `SessionUpdate` has plan and
  compaction arms; `NewSessionRequest` takes MCP servers; capability structs
  carry unstable sub-structs. Only **20** are genuinely out of reach.

So instability cannot be the axis. Three rules replace it:

**Generation scope follows reachability, not stability.** Layer 2 generates the
transitive closure of the operations that are implemented, and nothing else. It
is a mechanical rule with a mechanical check: regenerate, compute the closure,
and fail if the exported set differs. Adding an operation in layer 6 grows the
closure and the diff shows exactly what became public.

**A generated type is generated whole.** Every field, every union arm. Upstream's
marker warns that a shape may change; it is not permission to omit it. Dropping
an unstable arm of `SessionUpdate` would make this package fail to decode a
message an agent is entitled to send — the same defect as inventing an arm, in
the other direction.

**The compatibility promise carves them out, explicitly, before v1.0.** Otherwise
tagging v1.0 freezes `PlanUpdate` against a schema that says it may change at any
point, and the module is promising something upstream has not. Symbols whose
schema definition carries the UNSTABLE marker are exempt from the module's
compatibility guarantee, their Go doc comments repeat upstream's warning
verbatim, and `gorelease` findings against them are reviewed rather than
automatically blocking. Pre-1.0 this costs nothing — `gorelease` reports without
enforcing on `v0.x` — which is why it has to be written down now rather than
discovered at the v1.0 tag.

## What is deliberately not copied from the TypeScript SDK

- `AgentApp`/`ClientApp` and the `connectWith` builder
  (`acp-typescript-sdk/src/acp.ts:1841`, `:2093`). That is a framework on top of
  the protocol; the Go version is a `Config` struct and a `Connect` call.
- Zod schemas alongside types (`src/schema/zod.gen.ts`, 4,246 lines). Go decodes
  into the generated types, and the places needing more than the decoder can say
  get a hand-written check.
- The 4,252-line `acp.ts`. Most of its size is the two type sets and the handler
  routing tables that generation should be producing.

Middleware, which the MCP SDK exposes on both sides
(`go-sdk/design/design.md:405`), is also not in the first version. Dispatch is
routed through one internal handler type from the start so it can be added later
without an API break, but an extension point with nothing behind it is the
speculative abstraction [AGENTS.md](../AGENTS.md) rules out.
