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

Go has no sum type and the schema has 41 unions. They are not one problem; they
are two, and conflating them is how this goes wrong.

### The 14 string enumerations stay open

`StopReason`, `ToolKind`, `ToolCallStatus`, `Role`, `PlanEntryPriority` and the
rest are a closed set of strings on the wire.

```go
type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonMaxTokens StopReason = "max_tokens"
	StopReasonRefusal   StopReason = "refusal"
	StopReasonCancelled StopReason = "cancelled"
)
```

A named string, not an integer with a mapping table. A peer on a later protocol
revision will send a value not in this list, and a decoder that rejects it turns
"your editor is older than the agent" into "the connection is broken."
Unmarshalling keeps the unknown string; a `switch` handles what it knows and has
a `default`. This is why `.golangci.yml` sets
`exhaustive.default-signifies-exhaustive`.

### The 27 struct unions get a sealed interface and an unknown arm

These carry a discriminator and a payload. `SessionUpdate` is the one that
matters: 15 arms tagged by a `sessionUpdate` field, and it is the message the
agent sends continuously for the whole of a turn.

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
once, and the compiler helps. This is what the MCP SDK does for `Content`
(`go-sdk/mcp/content.go:22`), and it reserves the flat-struct form for small
unions where the arms genuinely share fields.

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

Without that arm, a sealed union means an editor built against `schema-v1.21.0`
silently discards whatever `schema-v1.22.0` adds. ACP adds arms — `SessionUpdate`
did not start at fifteen — so this is a certainty, not a hypothetical.
TypeScript gets the property free from structural typing; Go has to choose it.
Every struct union gets an unknown arm, and round-tripping an unknown arm
unchanged is a test rather than a hope.

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

## Handlers are fields, and they imply capabilities

The TypeScript SDK declares `interface Client` with fourteen methods, twelve
optional (`acp-typescript-sdk/src/acp.ts:3723`). Go has no optional interface
methods, and the transliteration — fourteen methods where twelve may return
"unsupported" — makes every caller write stubs and makes capability advertisement
a second list to keep in step by hand.

Instead, the MCP SDK's pattern (`go-sdk/mcp/client.go`, `ClientOptions`), which
fits ACP better than it fits MCP:

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
about, both stop being expressible. `Capabilities` remains the escape hatch for
the case where the two genuinely differ.

It fits ACP better than MCP because ACP gates almost everything: past four
baseline methods, every one of the remaining 39 is behind a capability.

This is a `Config` struct with useful zero values rather than functional options,
per [AGENTS.md](../AGENTS.md).

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

## Cancellation is not `ctx.Err()`

The subtlety most likely to be got wrong, and worth settling before any code
exists. The protocol requirement is in
[protocol.md](./protocol.md#cancellation): `session/cancel` does not end a turn.
The agent must still answer the outstanding `session/prompt` with `StopReason`
`cancelled`, and the client must keep reading `session/update` until it does.

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

Returning `ctx.Err()` at cancellation is the idiomatic-looking choice and would
drop the tail of every cancelled turn. The rule here is that the protocol's
semantics win and the doc comment carries the surprise.

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
- `IOTransport` — any `io.Reader`/`io.Writer` pair.
- `InMemoryTransport` — a client and an agent in one test with no subprocess. The
  reason the rest of the package never touches `os`.

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

## Stable only

Upstream ships v1 (`schema.json`, tag `schema-v1.21.0`) and an unstable v2
(`schema/v2/schema.unstable.json`, tag `schema-v2.0.0-alpha.3`), and the
TypeScript SDK carries both. This module implements v1 and nothing else until v2
stabilises. Two type sets in one package would double the generated surface and
grow every union arms that exist only in a draft. If v2 becomes necessary before
it is stable it goes in a separate package whose name says so.

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
