# Design

How this repository implements the Agent Client Protocol in Go, and why each
decision went the way it did.

The shape is taken from the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk),
which the Go team wrote and documented in its own `design/design.md`. That SDK
solves the same problem this one has — a bidirectional JSON-RPC protocol,
capability-gated optional methods, a large generated type set, and a spec that
will keep moving — and it solved it in Go rather than in a transliteration of the
TypeScript. Where ACP differs from MCP, this document says so and says what
changed.

## What the protocol actually is

Two peers over one bidirectional JSON-RPC 2.0 connection. A **client** is an
editor: it owns the workspace, the files, the terminals and the user. An
**agent** is a program that uses a model to read and change that workspace. The
client starts the agent as a subprocess and speaks newline-delimited JSON over
its stdin and stdout.

The published schema has 265 definitions and 43 methods: 28 the client calls on
the agent, 14 the agent calls on the client, and `$/cancel_request`. Almost all
of them past the first four are gated on a capability negotiated by
`initialize`.

That shape decides most of what follows. Two numbers in particular:

- **265 definitions** means the wire types are generated, not written. Nobody
  hand-maintains that against a moving upstream.
- **14 of the 43 methods run agent → client**, so this is not a client library
  with a server mode bolted on. Both directions are the same machinery seen from
  opposite ends, and the package is built that way from the start.

## Package layout

```text
github.com/Tangerg/acp                        package acp — the whole user-facing API
github.com/Tangerg/acp/jsonrpc                message types, for custom transports only
github.com/Tangerg/acp/internal/jsonrpc2      the connection machinery
github.com/Tangerg/acp/internal/cmd/schemagen the generator
schema/schema.json                            the vendored upstream schema
```

One package for the API, as `net/http`, `net/rpc` and the MCP SDK all do. The
alternative — `acp/client`, `acp/agent`, `acp/types` — splits a symmetric
protocol down a seam that isn't there: `RequestPermissionParams` is written by
the agent and read by the client, and there is no honest package for it. A
single package also means the decision never has to be revisited when the spec
adds a method that spans the seam.

`jsonrpc` is public only because a custom transport has to name the message type
it carries. Nothing else about JSON-RPC appears in the API: not request IDs, not
the envelope, not the method strings. Those are transport concerns and a caller
who has to know them has been handed the plumbing.

## The wire types are generated

Upstream ships `schema.json` and `meta.json` on a GitHub release tag
(`schema-v1.21.0` at the time of writing). The TypeScript SDK downloads them,
generates its types, and has a `--check` mode that CI runs to prove the checked-in
output still matches. Do the same:

- `schema/schema.json` is vendored and pinned to a release tag. The wire contract
  becomes a repository artifact that shows up in a diff when it moves.
- `internal/cmd/schemagen` reads it and emits the types.
- The output is committed, so `go get` needs no generator and pkg.go.dev has
  something to document.
- CI runs the generator and fails if the tree changes — the same promise as
  `go mod tidy -diff`.

Generated code stays inside the lint gate. `.golangci.yml` already turns off
golangci-lint's implicit exclusion of generated files, because these are the
types every caller touches and the ones most worth checking.

## Unions

Go has no sum type and the schema has 41 unions. They are not one problem; they
are two, and conflating them is how this goes wrong.

### String enumerations stay open

Seventeen of them — `StopReason`, `ToolKind`, `ToolCallStatus`, `Role`,
`PlanEntryPriority` — are just a closed set of strings on the wire.

```go
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonMaxTokens StopReason = "max_tokens"
	StopReasonRefusal   StopReason = "refusal"
	StopReasonCancelled StopReason = "cancelled"
)
```

A named string, not an integer with a mapping table. A peer running a later
protocol revision will send a value not in this list, and a decoder that rejects
it turns "your editor is older than the agent" into "the connection is broken."
Unmarshalling keeps the unknown string; a `switch` handles what it knows and has
a `default`. This is why `.golangci.yml` sets
`exhaustive.default-signifies-exhaustive`.

### Struct unions get a sealed interface and an unknown arm

The other twenty-four carry a discriminator and a payload. `SessionUpdate` is the
one that matters: fifteen arms, tagged by a `sessionUpdate` field, and it is the
message the agent sends continuously for the whole of a prompt turn.

```go
// A SessionUpdate is one of [UserMessageChunk], [AgentMessageChunk],
// [AgentThoughtChunk], [ToolCallStart], [ToolCallProgress], [PlanUpdate], … or
// [UnknownSessionUpdate].
type SessionUpdate interface {
	isSessionUpdate()
}
```

An interface rather than one flat struct with fifteen optional field groups. The
flat struct makes every unrepresentable combination constructible and pushes the
"which of these is set?" question onto every reader; a type switch answers it
once, and the compiler helps.

The interface is sealed with an unexported method, because the arms are the
protocol's and not the caller's. Sealing has a cost, and it is the important part
of this section:

```go
// UnknownSessionUpdate is a variant from a protocol revision this package does
// not implement. It keeps the message intact so that a client which forwards or
// records updates does not silently drop one a newer agent sent.
type UnknownSessionUpdate struct {
	Kind string
	Raw  json.RawMessage
}
```

Without that arm, a sealed union means an editor built against schema v1.21
silently discards whatever v1.22 adds. ACP adds arms — `SessionUpdate` already
grew to fifteen — so this is a certainty, not a hypothetical. TypeScript gets the
property for free from structural typing; Go has to decide to keep it. Every
struct union gets an unknown arm, and round-tripping an unknown arm unchanged is
a test, not a hope.

## Client and agent

The MCP SDK separates `Client` from `ClientSession`: the first holds the handlers
and features, the second is one logical connection, and one client may hold many.
That split is right and it carries over. Its name does not.

**ACP already means something by "session."** `session/new` returns a
`sessionId`; a session is a conversation with its own history, and one connection
carries many of them. Calling the connection a session too would leave
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
case that is almost all of them — serve one connection on stdio until the client
goes away.

Every spec method takes a context and a params pointer and returns a result
pointer and an error, with no convenience overloads. This is the MCP SDK's rule
and the reasoning transfers exactly: when the spec adds an optional field to a
request, a params struct absorbs it and a positional signature cannot. Passing
`nil` params is always valid for a method that currently needs none.

## Handlers are fields, and they imply capabilities

The TypeScript SDK declares `interface Client` with fourteen methods, twelve
optional. Go has no optional interface methods, and the transliteration —
fourteen methods where twelve may return "unsupported" — makes every caller
implement stubs and makes capability advertisement a second thing to keep in
step by hand.

Instead, the MCP SDK's pattern, which fits ACP better than it fits MCP:

```go
type ClientConfig struct {
	// Required. The agent may call these on any connection.
	RequestPermission func(context.Context, *RequestPermissionRequest) (*RequestPermissionResult, error)
	SessionUpdate     func(context.Context, *SessionUpdateRequest) error

	// Optional. Setting one advertises the capability that gates it.
	ReadTextFile   func(context.Context, *ReadTextFileRequest) (*ReadTextFileResult, error)
	WriteTextFile  func(context.Context, *WriteTextFileRequest) error
	CreateTerminal func(context.Context, *CreateTerminalRequest) (*CreateTerminalResult, error)

	// Capabilities overrides what the handlers imply.
	Capabilities *ClientCapabilities
}
```

The capability and the code that honours it become one decision. Advertising
`fs.readTextFile` with no reader, or writing a reader the agent is never told
about, both stop being possible to express. `Capabilities` stays as the escape
hatch for the case where the two genuinely differ.

This is a `Config` struct with useful zero values rather than functional options,
per [AGENTS.md](./AGENTS.md).

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

The TypeScript SDK reached the same conclusion with `TerminalHandle`. The handle
also gives the release obligation somewhere to live: a terminal that is never
released leaks a process on the client, and a type with a `Release` method is
where `defer` goes.

## Cancellation is not `ctx.Err()`

This is the subtlety most likely to be got wrong, and it is worth stating before
any code exists.

ACP has two cancellations. `$/cancel_request` cancels one JSON-RPC request.
`session/cancel` cancels a *prompt turn*, and the protocol is explicit about what
follows: the agent must still answer the original `session/prompt` with
`StopReason` `cancelled`, and the client must keep accepting `session/update`
notifications until it does, because the agent is entitled to send final tool-call
updates on the way out. A pending `session/request_permission` must be answered
`cancelled` rather than abandoned.

So `Prompt` cannot be a normal context-cancelled call:

```go
// Prompt runs one turn and blocks until it ends.
//
// If ctx is cancelled, Prompt sends session/cancel and keeps reading. The turn
// is over when the agent answers, not when the caller stops waiting: updates
// sent between the two are part of the turn's record, and the agent's own
// answer is what says how the turn ended. Prompt then returns a result whose
// StopReason is "cancelled", and a nil error — the cancellation succeeded.
//
// To stop without waiting, close the connection.
func (*ClientConn) Prompt(context.Context, *PromptParams) (*PromptResult, error)
```

Returning `ctx.Err()` at cancellation would be the idiomatic-looking choice and
would drop the tail of every cancelled turn. The rule this repository follows is
that the protocol's semantics win, and the doc comment carries the surprise.

## Transports

The MCP SDK's interface, unchanged, because it is the minimum a bidirectional
JSON-RPC link needs and it is the easiest thing for a caller to implement:

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
- `StdioTransport` — the agent side, on `os.Stdin` and `os.Stdout`.
- `IOTransport` — any `io.Reader`/`io.Writer` pair.
- `InMemoryTransport` — a client and an agent in one test with no subprocess. The
  reason the rest of the package never touches `os`.

HTTP and WebSocket are work in progress upstream and are not implemented until
they settle.

**An agent's stdout is the protocol stream.** One `fmt.Println` corrupts it, and
the failure arrives as an unrelated parse error at the other end. `StdioTransport`
therefore names the streams it uses rather than reaching for the globals, so that
the collision is visible in the code; the package documents the hazard and points
logging at stderr.

### JSON-RPC

Fork `golang.org/x/tools/internal/jsonrpc2_v2` into `internal/jsonrpc2`, which is
what the MCP SDK did. It is the implementation `gopls` has run in production for
years, it is already bidirectional, and it already handles request lifetime,
async calls and cancellation — the three things a hand-rolled version gets wrong
first. It is `internal` because it is an implementation detail; it is a fork
because the package is `internal` upstream and cannot be imported.

ACP's cancellation is `$/cancel_request`, LSP's spelling, not MCP's
`notifications/cancelled` — so the fork's cancellation wiring is closer to what
this needs than MCP's was.

## Stable only

Upstream ships v1 (`schema.json`) and an unstable v2
(`schema/v2/schema.unstable.json`), and the TypeScript SDK carries both. This
module implements v1 and nothing else until v2 stabilises. Two type sets in one
package would double the generated surface, and every union would grow arms that
exist only in a draft. If v2 becomes necessary before it is stable it goes in a
separate package whose name says so.

## Order of work

Each layer is a product that works end to end before the next one starts, per
[AGENTS.md](./AGENTS.md).

1. **Wire.** Vendored schema, generator, types, method constants. No I/O. Proven
   by round-tripping the schema's own shapes, including unknown union arms.
2. **Link.** The jsonrpc2 fork, `Transport`, `Connection`, `InMemoryTransport`. A
   client and an agent that complete `initialize` in one test.
3. **Turn.** `session/new`, `session/prompt`, `session/update`,
   `session/request_permission`, and cancellation as described above. This is the
   first useful release: an editor can drive an agent and a user can approve a
   tool call.
4. **Workspace.** The capability-gated optionals — `fs/*`, `terminal/*` — and
   `CommandTransport`, which is what makes it real against a published agent.
5. **The rest.** `session/load`, modes, elicitation, MCP passthrough, providers.

Middleware, which the MCP SDK exposes on both sides, is not in this list.
Dispatch is routed through one internal handler type from layer 2 so that it can
be added later without an API break, but an extension point with nothing behind
it is the speculative abstraction AGENTS.md rules out.

## What is deliberately not copied from the TypeScript SDK

- `AgentApp`/`ClientApp` and the `connectWith` builder. That is a framework on
  top of the protocol; Go's version is a `Config` struct and a `Connect` call.
- Zod schemas alongside types. Go decodes into the generated types, and the
  places that need more than the decoder can say get a hand-written check.
- The 4,252-line `acp.ts`. Its size is mostly the two type sets and the handler
  routing tables that generation should be producing.
