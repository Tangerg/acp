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

### Omitted, null and present are three states, not two

The schema distinguishes an absent property from one present as `null`, and it
does so **357 times**: that is how many optional-and-nullable property
occurrences are under `$defs`.

Go's ordinary answer, a pointer with `omitempty`, has only two states — a nil
pointer is both "absent" and "null" — so generated pointers cannot round-trip
this part of the grammar. The generator emits a presence-aware wrapper instead:

```go
// Opt distinguishes absent, null and present. IsZero reports absent, which is
// what the encoder's omitzero option consults, so an absent field emits nothing
// while an explicit null emits null.
type Opt[T any] struct { /* absent | null | present */ }

func (Opt[T]) IsZero() bool
func (Opt[T]) MarshalJSON() ([]byte, error)
func (*Opt[T]) UnmarshalJSON([]byte) error
```

used as `` `json:"cwd,omitzero"` ``. This depends on `encoding/json`'s `omitzero`
tag option, which consults `IsZero` and arrived in Go 1.24 — `omitempty` cannot
express it, because a struct wrapper is never empty by `omitempty`'s definition.
It is a second concrete reason the language floor is where
[repository.md](./repository.md#the-language-floor-is-125-not-the-newest-release)
puts it.

A state is collapsed only where the schema itself says the two are equivalent —
several capability fields document "omitted or `null` both mean not advertised" —
and that collapse is then a generated decision with the schema quoted beside it,
not a default.

### What the round-trip tests actually promise

An earlier draft promised that canonical valid values "round-trip
byte-identically". That is not a property `encoding/json` has, nor one the
TypeScript SDK has: whitespace, key order, number spelling and escapes may all
change while the decoded value is identical. Worse, it contradicted itself —
`x-deserialize-default-on-error` means a malformed field *must* come back as
something else.

So the promises are semantic, and there are four:

1. Both SDKs accept the same valid input and produce semantically equivalent
   JSON.
2. Both SDKs apply schema-directed recovery to the same normalised value.
3. Values constructed in Go encode to stable Go-owned golden output.
4. `_meta` and the extra properties of an open union survive as equivalent JSON
   values.

Not byte identity, even for `_meta`. The TypeScript SDK parses `_meta` through
`z.record(z.string(), z.unknown())` and reattaches parsed values, so the original
bytes are gone on that side too and no cross-SDK test could assert them. Go may
still hold `json.RawMessage` where that is the simplest lossless representation —
but then raw retention is an implementation property, not a promise the schema or
the oracle can back.

## The API

One place. Everything below this section explains a decision recorded here and
does not restate a signature — the previous revision left three mutually
exclusive sketches in three sections, which
[design-review-2.md](./design-review-2.md#1-the-documents-still-specify-three-incompatible-connection-apis)
caught. If prose and this table disagree, this table is wrong and must be fixed
here rather than contradicted there.

**Scope: authoritative through layer 5.** It shows every operation layers 1–5 of
[roadmap.md](./roadmap.md) implement and deliberately no others — `ResumeSession`,
`session/close`, `session/set_config_option`, `elicitation/*`, `providers/*` and
the rest arrive with layer 6 and are added here then. A partial table cannot also
be the final authority, so it says which surface it is final for. The exported
API is not frozen until this table covers everything a release claims.

```go
// Construction. Returns an error because the capability invariant is checked
// here: a configuration whose advertisement exceeds its implementation, or that
// omits a baseline handler, is rejected before it can accept a request it
// cannot serve.
func NewClient(*ClientConfig) (*Client, error)
func NewAgent(*AgentConfig) (*Agent, error)

// Client.Connect performs the handshake; Agent.Connect accepts one. The two are
// not symmetric and their milestones are defined under "Connecting" below.
func (*Client) Connect(context.Context, Transport) (*ClientConn, error)
func (*Client) Conns() iter.Seq[*ClientConn]
func (*Agent) Connect(context.Context, Transport) (*AgentConn, error)
func (*Agent) Run(context.Context, Transport) error
func (*Agent) Conns() iter.Seq[*AgentConn]

// Peer returns an immutable snapshot of what initialize negotiated. It is a
// copy: the same value backs the capability gate, and a caller must not be able
// to widen its own authority by mutating it.
func (*ClientConn) Peer() PeerInfo
func (*ClientConn) Authenticate(context.Context, *AuthenticateParams) (*AuthenticateResult, error)
func (*ClientConn) NewSession(context.Context, *NewSessionParams) (*ClientSession, *NewSessionResult, error)
func (*ClientConn) LoadSession(context.Context, *LoadSessionParams) (*ClientSession, *LoadSessionResult, error)
func (*ClientConn) Call(context.Context, string, any, any) error   // extension methods only
func (*ClientConn) Notify(context.Context, string, any) error      // extension methods only
func (*ClientConn) Close() error
func (*ClientConn) Wait() error

// The agent side of a connection is not a mirror of the client side: it serves
// rather than drives, so it has no session-creating methods. Sessions reach an
// agent through its handlers.
func (*AgentConn) Peer() PeerInfo
func (*AgentConn) Call(context.Context, string, any, any) error
func (*AgentConn) Notify(context.Context, string, any) error
func (*AgentConn) Close() error
func (*AgentConn) Wait() error

// The conversation, as the client sees it. Binds SessionID and nothing else.
func (*ClientSession) ID() SessionID
func (*ClientSession) Conn() *ClientConn
func (*ClientSession) Prompt(context.Context, *PromptParams) (*PromptResult, error)
func (*ClientSession) Cancel(context.Context, *CancelParams) error
func (*ClientSession) SetMode(context.Context, *SetModeParams) (*SetModeResult, error)

// The same conversation as the agent sees it: a different set of operations, so
// a different type. Handlers receive it; see "How an agent gets a session".
func (*AgentSession) ID() SessionID
func (*AgentSession) Conn() *AgentConn
func (*AgentSession) Update(context.Context, *SessionUpdateParams) error
func (*AgentSession) RequestPermission(context.Context, *RequestPermissionParams) (*RequestPermissionResult, error)
func (*AgentSession) ReadTextFile(context.Context, *ReadTextFileParams) (*ReadTextFileResult, error)
func (*AgentSession) WriteTextFile(context.Context, *WriteTextFileParams) (*WriteTextFileResult, error)
func (*AgentSession) CreateTerminal(context.Context, *CreateTerminalParams) (*Terminal, *CreateTerminalResult, error)

// Every one of these is a JSON-RPC request whose schema response is an object
// with optional _meta, so every one returns a result. See below.
func (*Terminal) ID() TerminalID
func (*Terminal) Output(context.Context, *TerminalOutputParams) (*TerminalOutputResult, error)
func (*Terminal) WaitForExit(context.Context, *WaitForExitParams) (*WaitForExitResult, error)
func (*Terminal) Kill(context.Context, *KillTerminalParams) (*KillTerminalResult, error)
func (*Terminal) Release(context.Context, *ReleaseTerminalParams) (*ReleaseTerminalResult, error)
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

**An empty-looking response is not a discardable one.** `Kill` and `Release`
returned only `error` in the previous revision, and
`KillTerminalResponse` and `ReleaseTerminalResponse` are objects carrying
optional `_meta` — so that signature threw away peer data, breaking the
"response is returned, not absorbed" rule two sections below it. Only
notifications return a bare `error`: `session/cancel` and `session/update` have
no response to return. Every request returns its generated result, however empty
that result looks today, and every future empty-object response is held to the
same rule.

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

Sentinels are `error` values backed by an unexported immutable type, not
exported `*Error` structs — a package-level `var ErrAuthRequired = &Error{...}`
is writable by any importer, and one that mutated it would silently change how
every other importer's `errors.Is` behaved. Peer errors remain `*Error` and stay
reachable with `errors.As`; `(*Error).Is` compares codes, which is what makes a
sentinel match one. There is a sentinel only where control flow needs one —
`ErrAuthRequired` today — because the `ErrorCode` constants plus `errors.As`
already cover ordinary inspection, and a sentinel per code would be API surface
nobody asked for. `-32000` is the one that
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
When `Cancel` is called, the connection **synchronously claims** those pending
requests — before the notification goes out and before `Cancel` returns — then
cancels their handler contexts and resolves each response exactly once. Claiming
first is what makes the race decidable: a user decision arriving afterwards finds
the request already answered and is dropped, rather than racing a resolution that
has not happened yet.

**The agent side owns one context per turn.** Receiving `session/cancel` cancels
the turn's context and the work descending from it; other sessions and unrelated
calls are untouched. Receiving `$/cancel_request` cancels that request and work
nested under it, but makes no promise about the `session/cancel` result shape.

The tree is connection → session → turn → request, with ordinary Go semantics: a
parent's cancellation cascades to its descendants, and a child's cancellation
touches neither its parent nor its siblings. The previous revision wrote "each
level is cancelled only by its own signal", which is not how `context` works and
would have described a tree where cancelling a connection left its turns running.

**Request cancellation is four steps, not one.** When a call's context finishes,
the connection retires the local pending call, returns the exact `ctx.Err()`,
sends `$/cancel_request` on an *independent bounded* context so an unresponsive
peer cannot hold the caller past its own deadline, and discards any late response
without reviving the retired call. That is the MCP SDK's behaviour
(`go-sdk/mcp/transport.go:281`) and all four parts are load-bearing. The
TypeScript SDK is cited above for *which message* per-request cancellation sends,
not for returning locally: it settles its promise from the peer's response.

**One prompt at a time per session.** `CancelNotification` carries only
`sessionId` — no turn or request identifier — so the protocol has no way to say
*which* turn a `session/cancel` means. That is only coherent if a session has at
most one active turn, so a second concurrent `Prompt` on the same session is
refused locally with a documented error rather than left to scheduling. Pending
permission requests are then indexed by session without ambiguity.

`ClientSession` and `AgentSession` are themselves safe for concurrent use; that
is a separate question from whether two prompts may overlap, and the answer to
the first is yes while the answer to the second is no.

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

Both configs carry a `Logger *slog.Logger`, because two paths have nowhere else
to report: a handler error is deliberately not sent to the peer in full, and a
notification handler has no response channel at all. **`nil` means discard** —
`slog.New(slog.DiscardHandler)`, not "log somewhere sensible". The library never
installs a default that writes to stdout, and this is not a style preference: an
agent's stdout is the protocol stream, so a well-meant default logger would
corrupt every connection it was supposed to help debug.

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

## The agent side

`ClientConfig` had a sketch and `AgentConfig` had none, which left half of a
symmetric protocol undescribed. The agent's config is the mirror in structure and
not in content: it serves rather than drives, so its fields are the operations a
client calls on it.

```go
type AgentConfig struct {
	Info    *Implementation
	Meta    map[string]any
	Logger  *slog.Logger // nil means discard; see below

	// Baseline. NewAgent fails if any is nil, because an agent that cannot
	// answer these cannot complete a connection.
	Initialize func(context.Context, *InitializeRequest) (*InitializeResult, error)
	NewSession func(context.Context, *NewSessionRequest) (*NewSessionResult, error)
	Prompt     func(context.Context, *AgentSession, *PromptRequest) (*PromptResult, error)
	Cancel     func(context.Context, *AgentSession, *CancelRequest)

	// Gated. Setting one advertises the capability that gates it; the grouping
	// follows the capability type, exactly as on the client side.
	LoadSession func(context.Context, *AgentSession, *LoadSessionRequest) (*LoadSessionResult, error)
	Auth        *AuthHandlers

	// Extension methods the generated table does not name.
	CallFallback   func(context.Context, *ExtRequest) (json.RawMessage, error)
	NotifyFallback func(context.Context, *ExtNotification)

	// nil: advertise what the handlers support. Non-nil: the complete desired
	// advertisement. Never inferred from the client callbacks an agent happens
	// to be able to make — what an agent advertises is what it implements.
	Capabilities *AgentCapabilities
}
```

`ClientConfig` gains the same `Logger`, `CallFallback` and `NotifyFallback`
fields, so the extension contract is symmetric: both directions can send
extension messages and both can receive them.

### How an agent gets a session

An agent never constructs an `*AgentSession`; the connection hands one to the
handlers whose requests carry a `sessionId`, which is why `Prompt` and `Cancel`
take one and `Initialize` does not. The handle is valid for as long as the
session is, not merely for the handler call — an agent that spawns work for a
turn keeps the handle to send `session/update` from it, and that is the ordinary
case rather than an escape. What it must not outlive is the connection: calling
through it after `Close` returns `ErrConnectionClosed`.

`NewSession` does not receive a handle because it is the call that creates one.
The connection builds the handle from the `sessionId` in the result the handler
returns.

## Connecting

"Connecting performs initialize" was written once for both sides and cannot be
true of both: a client initiates the handshake and an agent accepts it. The MCP
SDK has the same asymmetry, and stating it is cheaper than discovering it.

**`Client.Connect` returns only after** the transport connects, `initialize`
succeeds, the negotiated protocol version is one this package implements, and the
peer's capabilities are stored as an immutable snapshot. If any of those fails —
including the context being cancelled mid-handshake — it closes the logical
connection before returning, so a failed `Connect` never leaks a live transport
or a half-initialized peer.

**`Agent.Connect` returns once the read loop is running**, before any client has
sent `initialize`. It cannot wait for a handshake it does not control. Until
`initialize` arrives the connection answers every other method with
`-32600 Invalid request`; the only legal inbound message before it is
`initialize` itself, and a second `initialize` on the same connection is also
`-32600`.

The context passed to `Connect` scopes **setup only**, on both sides. It does not
own the connection's lifetime — a caller who passed a five-second handshake
timeout has not asked for the connection to die after five seconds. Lifetime is
owned by `Close`, and observed by `Wait`. `Agent.Run` is the exception and says
so: it is `Connect` then `Wait` then `Close`, and its context owns the whole run.

`Wait` returns the connection's terminal error: nil for a local `Close` or a
clean EOF, and the read or write failure otherwise. It is safe to call
concurrently and from many goroutines, and every caller gets the same value every
time — a terminal condition that reported differently depending on who asked
first would be unusable for deciding whether to reconnect.

`Peer()` returns a copy rather than the stored snapshot. The same value backs the
capability gate, and a caller who could mutate it could widen its own authority.

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

- Handle-facing **projections** — `PromptParams`, `TerminalOutputParams` — are
  exported, together with every result and domain type reachable from a public
  operation.
- The **complete wire request** behind a projection stays unexported when its
  identifiers can only come from a handle. Exporting both `PromptParams` and
  `PromptRequest` would publish two types for one operation, one of which no
  ordinary caller can validly construct. A complete request is exported only when
  it is itself part of a public handler contract — `*PromptRequest` is what an
  agent's handler receives — and cannot be replaced by a direction-specific
  projection.
- Other method results and notifications, and every domain type they reach, are
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

The public `jsonrpc` package needs enough surface to be usable. Handing a
transport an opaque `jsonrpc.Message` with no way to turn it into bytes is not an
extension point; the MCP SDK exports `EncodeMessage`, `DecodeMessage` and
`MakeID` beside its type aliases (`go-sdk/jsonrpc/jsonrpc.go:28`). This package
exports the message type aliases and:

```go
func EncodeMessage(Message) ([]byte, error)
func DecodeMessage([]byte) (Message, error)
```

and no more. A byte-stream transport frames and unframes; it does not mint
request IDs, so `MakeID` is not exported until something needs it. Widening a
package is a minor release; narrowing one is not, so the smaller set is the one
to start from — but it has to be at least this large, or the `Transport`
interface is decorative.

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
