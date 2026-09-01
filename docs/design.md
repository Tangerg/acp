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

- **170 definitions** means the wire types are generated, not written. Nobody
  hand-maintains that against a moving upstream.
- **11 of the 25 methods run agent → client**, so this is not a client
  library with a server mode bolted on. Both directions are the same machinery
  seen from opposite ends, and the package is built that way from the first
  commit.

## Package layout

```text
github.com/Tangerg/acp                        package acp — the whole user-facing API
github.com/Tangerg/acp/jsonrpc                message types, for custom transports only
github.com/Tangerg/acp/internal/wire          the runtime the generated codecs are written against
github.com/Tangerg/acp/internal/jsonrpc2      the connection machinery
github.com/Tangerg/acp/internal/cmd/schemagen the generator
schema/schema.json  schema/meta.json          the vendored upstream schema
schema/manifest.json                          the generation roots
schema/exported.txt                           what their closure became, for the gate to check
schema.gen.go  methods.gen.go                 the generated types, and the method table
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

`internal/wire` is where the schema's decoding semantics live: splitting an
object so one property can fail on its own, the two array decoders that
`x-deserialize-skip-invalid-items` needs, the discriminant reader and splicer,
retained catch-all properties, and JSON pointer paths for failures. It names no
method, no capability and no message type, and it imports nothing from `acp` —
which is what lets the generated code, which names all of those, sit in the
exported package and still share one runtime. The alternative was 170 restatements
of the same six rules.

## The wire types are generated

Upstream publishes `schema.json` and `meta.json` as assets on a release tag. The
TypeScript SDK downloads them, generates its types, and has a `--check` mode CI
runs to prove the committed output still matches
(`acp-typescript-sdk/scripts/generate.js:450` for the download,
`package.json`'s `generate:check` for the gate). Do the same:

- `schema/schema.json` is vendored and pinned to a release tag, currently
  `schema-v1.21.0`. The wire contract becomes a repository artifact that shows up
  in a diff when it moves.
- `github.com/google/jsonschema-go/jsonschema`, the package extracted from the
  MCP Go SDK, checks the document's standard JSON Schema structure, references,
  and defaults before generation.
- `internal/cmd/schemagen` applies the ACP-specific `x-deserialize-*`, `x-method`,
  and `x-side` rules and emits the types.
- The output is committed, so `go get` needs no generator and pkg.go.dev has
  something to document.
- CI runs the generator and fails if the tree changes — the same promise as
  `go mod tidy -diff`.

Generated code stays inside the lint gate. `.golangci.yml` already disables
golangci-lint's implicit exclusion of generated files, because these are the
types every caller touches and so the ones most worth checking.

The public elicitation types remain generated from ACP's narrower schema rather
than becoming aliases for the general-purpose `jsonschema.Schema`. The latter
accepts the full JSON Schema vocabulary; exposing it on the ACP wire would allow
messages the protocol does not permit. The shared package owns the standard,
while ACP's generated types own the protocol grammar.

## Unions

Go has no sum type and the schema has 32 unions. An earlier draft of this page
split them into "14 open string enumerations and 27 discriminated struct unions"
and gave every struct union an unknown arm. Both halves of that were wrong, and
[design-review.md](./design-review.md#1-the-union-model-disagrees-with-the-v1-schema)
is why this section was rewritten. The correction matters because it inverts the
forward-compatibility rule: the schema, not this package, decides which unions
are open.

They include closed enumerations, open string unions, discriminated object
unions, and primitive/value unions. The schema says which is which explicitly;
the generator preserves that decision instead of adding a universal unknown arm.

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
// A SessionUpdate is one of [*UserMessageChunk], [*AgentMessageChunk],
// [*AgentThoughtChunk], [*ToolCall], [*ToolCallUpdate], [*Plan], …
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

### The discriminant belongs to the union, not to the arm

Writing an object union needs a decision the previous revision never made: which
type writes the `"sessionUpdate": "tool_call"` property. Counting settles it.

**`ContentChunk` is the payload of three different `SessionUpdate` arms** —
`user_message_chunk`, `agent_message_chunk` and `agent_thought_chunk` — so a
payload that wrote its own tag could not serve all three. **`ToolCallUpdate` is
both a `SessionUpdate` arm and an ordinary property of
`RequestPermissionRequest`**, so a payload that wrote a tag would corrupt the
second use. Either case alone rules the idea out.

So the union's codec writes the tag, by splicing it as the first property of the
arm's encoded object, and reads it before selecting an arm. Splicing is safe
without re-parsing the arm's output because the generator refuses to emit an arm
whose payload declares a property with the discriminant's name — the check is at
generation time, not on every message.

That leaves arm naming, which follows from the same counting:

- **An arm is its payload's own Go type** when it carries exactly one `$ref`
  payload, declares nothing of its own but the discriminant, and no other arm
  anywhere in the schema shares that payload. This is the ordinary case, and it
  is what makes `ContentBlock`'s arms `TextContent` and `ImageContent` rather than
  something invented.
- **Otherwise the arm gets a generated type**, named after the arm — its `title`,
  or its discriminant value — and qualified by the union's name when that is
  already taken. `SessionUpdate`'s three chunk arms become `UserMessageChunk`,
  `AgentMessageChunk` and `AgentThoughtChunk`; `MultiSelectItems`'s catch-all
  becomes `MultiSelectItemsOther`, because `ElicitationPropertySchema` has a
  catch-all titled `other` too.

Names are allocated over the **whole** schema, not over the manifest's closure.
Otherwise growing the manifest would rename a type that was already published,
and the rename would arrive as a surprise in a compatibility report rather than
as a decision.

### Selecting an arm

One algorithm covers all four union shapes, and it is the reference
implementation's:

1. Read the discriminant. A property that is present but **not a string is not a
   discriminant** — including `null`, which `json.Unmarshal` would otherwise
   accept into a string and turn into the empty-string tag.
2. A discriminant a known arm claims selects that arm, and a payload that does
   not match it **fails**. It does not fall through. This is the `not` clause of
   the open unions' catch-all arm, and it holds for closed unions too.
3. A discriminant no arm claims lands in the catch-all if the union has one.
4. Otherwise the arms that declare no discriminant are tried in schema order,
   first match wins, which is what `anyOf` asks for. `EmbeddedResourceResource`
   has only these: its two arms are told apart by which property they require.
5. Nothing left to try is a decode error.

### Two union shapes the arm model has to bend for

Twenty-three unions in the implemented closure are the discriminated object kind
above. Two others are not, and each needs a rule of its own.

**A value union's arms are different JSON shapes**, so there is no discriminant to
read: `RequestId` is null, an integer or a string, and
`SessionConfigSelectOptions` is one of two differently-typed arrays. The arms are
still Go types — only a named type can implement the union's interface — and a
value is offered to each in schema order, first match wins. The null arm goes
first, because every other arm's decoder refuses null.

Their arms are named `<Union><Arm>` rather than `<Arm>`, unlike an object union's.
An object union's arms are named after domain concepts and stand alone;
a value union's titles are shape words — `Ungrouped`, `Str`, `Number` — which
would be poor names in a package of their own and would collide with the next
schema release that wanted one.

**A flattened union is an object and a union at once**: `SessionConfigOption` has
five properties of its own plus one of two kind-specific shapes, all in the same
JSON object. A Go struct cannot be several shapes at once, so the struct holds the
choice in a `Value` field of a generated `<Struct>Value` interface, and the
encoder splices the two halves back into one object. The name is generated because
the schema gives this construct none: it is an object and a `oneOf` side by side.

## Validation is part of the codec, not an afterthought

`encoding/json` checks JSON syntax and Go field types. It does not implement this
schema. The v1 schema uses two extension keywords heavily —
**249 occurrences of `x-deserialize-default-on-error`** and **27 of
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

The last two are **not a separate pass**, and that is what makes them true rather
than aspirational. Validation is inside `MarshalJSON` and `UnmarshalJSON`, on the
type that owns the rule, so it happens wherever a value crosses the boundary:

- A closed enumeration checks membership in both directions. Decoding cannot
  produce an undefined value, and encoding cannot send one — which matters,
  because building one in Go is the only way to have one.
- A required property's absence is a decode failure, and a required array encodes
  as `[]` rather than the `null` a nil Go slice would produce.
- A union refuses a nil arm rather than sending `null` where an object is
  required, and a catch-all arm refuses a discriminant a known arm reserves.
- A numeric bound is enforced by the Go type the schema's `format` picks, so
  `ProtocolVersion` is a `uint16` and its declared range of 0 to 65535 needs no
  check. Every `minimum` in the schema is the low bound of an unsigned format and
  the single `maximum` is a `uint16`'s, so the generator asserts this rather than
  assuming it: a bound the Go type does not already guarantee stops generation.

A caller who never touches the connection layer therefore still gets checked, and
there is no second code path that could disagree with the first.

### What recovery recovers to

`x-deserialize-default-on-error` says a malformed property does not fail the
message, but not what it becomes. The schema does not say either, and the answer
is three rules — read off the reference implementation, because "the same
normalised value" is a promise about behaviour and not about intent:

1. The declared `default`, if the property has one. `ClientCapabilities.fs`
   becomes `{readTextFile: false, writeTextFile: false}`.
2. Otherwise an empty array, if the property is an array that cannot be null.
3. Otherwise nothing: the property arrives absent.

A declared `default` is also what an **absent** property becomes, which is a
separate rule and applies whether or not the property recovers. It is why a
defaulted optional property needs no `Opt`: absence already has a value.

Rule 2 says "cannot be null" where the reference implementation tests whether
`type` is the single name `"array"` — a nullable array's `["array", "null"]` fails
that test. The two readings agree on every property in the schema, and this one is
the honest version of the same rule: an array that may be null has somewhere else
to go.

### Two property shapes with no type to speak of

`AuthMethodTerminal.env` is `additionalProperties` carrying a schema, which is a
map: `map[string]string`. There is no key type to resolve, because JSON object
keys are strings.

`Error.data` has no `type` at all, which admits every JSON value including null.
It is `json.RawMessage`, and it is treated as nullable however the schema spells
that — which is not at all — because a property that admits everything admits
null, and null there is a value rather than an absence.

### Omitted, null and present are three states, not two

The schema distinguishes an absent property from one present as `null`, and it
does so **222 times**: that is how many optional-and-nullable property
occurrences are under `$defs`.

Go's ordinary answer, a pointer with `omitempty`, has only two states — a nil
pointer is both "absent" and "null" — so generated pointers cannot round-trip
this part of the grammar. The generator emits a presence-aware wrapper instead:

```go
// Opt distinguishes absent, null and present. The zero Opt is absent.
type Opt[T any] struct { /* absent | null | present */ }

func OptValue[T any](v T) Opt[T] // present
func OptNull[T any]() Opt[T]     // present as null

func (Opt[T]) Get() (T, bool) // the value, and whether there is one
func (Opt[T]) IsZero() bool   // absent; what omitzero consults
func (Opt[T]) IsNull() bool   // present as null
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

An `Opt` is also not reached for where a plain Go type already has the states the
property has. A property that cannot be null and has a declared default takes that
default when absent, so it has one state and is a plain `T`. An optional array
that cannot be null has two, and a slice already spells them: nil is absent and
empty is present, which `omitzero` encodes correctly and `omitempty` could not.

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

And one thing the corpus cannot promise, because it turned out not to be true:
that this package and the reference implementation agree on everything the schema
states. **They disagree about integers.** The reference implementation validates
an `int64` or `int32` property as a plain number, reaching for an integer check
only where the schema also states a minimum — so most of its integer properties
accept `1.5`. The schema types them `integer`, so this package refuses the value,
and [AGENTS.md](../AGENTS.md) settles which is right: the schema wins, and the
difference belongs upstream rather than in a local dialect.

So a fixture may carry a **divergence**: what this package does instead, and the
schema clause that decides it. The mechanism is deliberately awkward to use — the
divergence has to be a real one and has to name a clause, or the test refuses it —
because a way to make any failing case pass by asserting that this package is
right would be worse than no oracle at all.

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
`session/close`, `session/set_config_option`, `elicitation/*` and
the rest arrive with layer 6 and are added here then. A partial table cannot also
be the final authority, so it says which surface it is final for. The exported
API is not frozen until this table covers everything a release claims.

```go
// Construction. Returns an error because the capability invariant is checked
// here: a configuration whose advertisement exceeds its implementation, or that
// omits a baseline handler, is rejected before it can accept a request it
// cannot serve.
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
func (*ClientConn) Authenticate(context.Context, *AuthenticateRequest) (*AuthenticateResponse, error)
func (*ClientConn) NewSession(context.Context, *NewSessionRequest) (*ClientSession, *NewSessionResponse, error)
func (*ClientConn) LoadSession(context.Context, *LoadSessionRequest) (*ClientSession, *LoadSessionResponse, error)
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
func (*ClientSession) Prompt(context.Context, *PromptParams) (*PromptResponse, error)
func (*ClientSession) Cancel(context.Context, *CancelParams) error
func (*ClientSession) SetMode(context.Context, *SetModeParams) (*SetSessionModeResponse, error)

// The same conversation as the agent sees it: a different set of operations, so
// a different type. Handlers receive it; see "How an agent gets a session".
func (*AgentSession) ID() SessionID
func (*AgentSession) Conn() *AgentConn
func (*AgentSession) Update(context.Context, *SessionUpdateParams) error
func (*AgentSession) RequestPermission(context.Context, *RequestPermissionParams) (*RequestPermissionResponse, error)
func (*AgentSession) ReadTextFile(context.Context, *ReadTextFileParams) (*ReadTextFileResponse, error)
func (*AgentSession) WriteTextFile(context.Context, *WriteTextFileParams) (*WriteTextFileResponse, error)
func (*AgentSession) CreateTerminal(context.Context, *CreateTerminalParams) (*TerminalHandle, *CreateTerminalResponse, error)

// Every one of these is a JSON-RPC request whose schema response is an object
// with optional _meta, so every one returns a result. See below.
func (*TerminalHandle) ID() TerminalID
func (*TerminalHandle) Session() *AgentSession
func (*TerminalHandle) Output(context.Context, *TerminalOutputParams) (*TerminalOutputResponse, error)
func (*TerminalHandle) WaitForExit(context.Context, *WaitForTerminalExitParams) (*WaitForTerminalExitResponse, error)
func (*TerminalHandle) Kill(context.Context, *KillTerminalParams) (*KillTerminalResponse, error)
func (*TerminalHandle) Release(context.Context, *ReleaseTerminalParams) (*ReleaseTerminalResponse, error)

// Transports. A connection is established over one of these, and a caller may
// implement its own: the contract is in "Transports" below.
func NewInMemoryTransports() (client, agent Transport)
func NewStdioTransport() Transport
func NewIOTransport(io.ReadCloser, io.WriteCloser) Transport
func NewCommandTransport(*CommandConfig) Transport
```

Two spellings in the previous revision did not survive contact with the schema,
and the schema wins both. A response type is `…Response` and not `…Result`,
because that is what the schema calls it. And the handle for a terminal is
`TerminalHandle`, because `Terminal` is already the name of a definition — the
payload of a tool call's terminal content — and the schema's names are not this
package's to reassign.

Every operation takes `(ctx, *Params)` and returns `(*Response, error)`, with no
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
`(*ClientSession, *NewSessionResponse, error)`. Three results is mildly ugly and
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

**The connection owns permission requests that arrive during an active turn,
indexed by session.** The schema does not make a permission request invalid when
no prompt is running; such a request is dispatched but remains outside the turn
aggregate, where its own `$/cancel_request` still applies. When `session/cancel`
is sent or received, every pending turn-owned `session/request_permission` for
that session must be answered with the `cancelled` outcome — while the user's
handler may still be blocked on a dialog.
When `Cancel` is called, the connection **synchronously claims** those pending
requests — before the notification goes out and before `Cancel` returns — then
cancels their handler contexts and resolves each response exactly once. Claiming
first is what makes the race decidable: a user decision arriving afterwards finds
the request already answered and is dropped, rather than racing a resolution that
has not happened yet. If the permission handler already claimed its response,
`Cancel` waits for that response write to settle before sending `session/cancel`;
claiming an answer is not the same as putting it on the wire.

**The agent side owns one context per turn.** Receiving `session/cancel` cancels
the turn's context and the work descending from it; other sessions and unrelated
calls are untouched. Receiving `$/cancel_request` cancels that request and work
nested under it, but makes no promise about the `session/cancel` result shape.

The tree is connection → session → turn → request, with ordinary Go semantics: a
parent's cancellation cascades to its descendants, and a child's cancellation
touches neither its parent nor its siblings. The previous revision wrote "each
level is cancelled only by its own signal", which is not how `context` works and
would have described a tree where cancelling a connection left its turns running.

**Request cancellation is four steps, not one.** When an ordinary call's context
finishes, the connection retires the local pending call, returns the exact
`ctx.Err()`, sends `$/cancel_request` on an *independent bounded* context so an
unresponsive peer cannot hold the caller past its own deadline, and discards any
late response without reviving the retired call. `Prompt` keeps its pending call
only to observe when the turn is free again after its caller leaves; that internal
obligation does not delay the caller's return. This follows the MCP SDK's
behaviour for ordinary calls
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
	Meta Meta
	// nil means discard. See below.
	Logger *slog.Logger

	// A notification has no response, so its handler returns nothing. A request
	// handler returns a response and an error, which becomes the peer's error.
	SessionUpdate     func(context.Context, *SessionNotification)
	RequestPermission func(context.Context, *RequestPermissionRequest) (*RequestPermissionResponse, error)

	ReadTextFile  func(context.Context, *ReadTextFileRequest) (*ReadTextFileResponse, error)
	WriteTextFile func(context.Context, *WriteTextFileRequest) (*WriteTextFileResponse, error)
	Terminal      *TerminalHandlers // all five, or none

	// Extension methods the generated table does not name.
	CallFallback   func(context.Context, *ExtRequest) (json.RawMessage, error)
	NotifyFallback func(context.Context, *ExtNotification)

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
annotates method payloads with `x-method` and `x-side` — 46 occurrences each —
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
	Info   *Implementation
	Meta   Meta
	Logger *slog.Logger

	// What an agent offers a client that must authenticate. Empty says none is
	// needed.
	AuthMethods []AuthMethod

	// Baseline. NewAgent fails if any is nil, because an agent that cannot
	// answer these cannot complete a turn.
	NewSession func(context.Context, *NewSessionRequest) (*NewSessionResponse, error)
	Prompt     func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error)
	Cancel     func(context.Context, *AgentSession, *CancelNotification)

	// Optional or gated. Setting LoadSession advertises the capability that
	// gates it; the grouping follows the capability type, exactly as on the
	// client side.
	Authenticate func(context.Context, *AuthenticateRequest) (*AuthenticateResponse, error)
	LoadSession  func(context.Context, *AgentSession, *LoadSessionRequest) (*LoadSessionResponse, error)
	SetMode      func(context.Context, *AgentSession, *SetSessionModeRequest) (*SetSessionModeResponse, error)

	CallFallback   func(context.Context, *ExtRequest) (json.RawMessage, error)
	NotifyFallback func(context.Context, *ExtNotification)

	// nil: advertise what the handlers support. Non-nil: the complete desired
	// advertisement. Never inferred from the client callbacks an agent happens
	// to be able to make — what an agent advertises is what it implements.
	Capabilities *AgentCapabilities
}
```

**There is no `Initialize` handler**, and the previous revision's listing one was
two sources of truth for one answer. Everything the initialize response carries —
the version, the capabilities, the auth methods, the identification — is already
in this struct, so a handler for it could only return what the struct says or
contradict it. The connection answers initialize itself, which is also the one place a second
initialize can be refused. An agent that wants to see what the client
advertised asks `Peer()`.

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
`initialize` itself. Initialize attempts are serialized in wire order. Once one
is accepted, every later attempt is `-32600`; if parameter validation rejects an
attempt before acceptance, its response settles before the next queued or later
attempt can negotiate. The client may therefore correct the request and retry
without reconnecting, while two attempts can never negotiate concurrently.

The context passed to `Connect` scopes **setup only**, on both sides. It does not
own the connection's lifetime — a caller who passed a five-second handshake
timeout has not asked for the connection to die after five seconds. Lifetime is
owned by `Close`, and observed by `Wait`. `Agent.Run` is the exception and says
so: it is `Connect` then `Wait` then `Close`, and its context owns the whole run.

`Wait` returns the connection's terminal error: nil for a local `Close` or a
clean EOF, and the read or terminal write failure otherwise. A transport may
return exactly the caller's `ctx.Err()` to report that no part of a message was
committed; that operation fails without ending the connection. It is safe to call
concurrently and from many goroutines, and every caller gets the same value every
time — a terminal condition that reported differently depending on who asked
first would be unusable for deciding whether to reconnect.

`Peer()` returns a copy rather than the stored snapshot. The same value backs the
capability gate, and a caller who could mutate it could widen its own authority.

## What order inbound messages are served in

The connection makes two ordering promises, and both are load-bearing.

**A notification is served in arrival order, one at a time.** `session/update` is
a stream — message chunks, tool calls, plans, in the order the agent produced
them — so handling two of them concurrently would deliver a turn's output
scrambled.

**A response is delivered only after every notification that arrived before it.**
Without that, `Prompt` could return while the last chunk of the turn it describes
was still queued, and a caller would see a turn end before hearing how it ended.
The specification puts those updates before the response on the wire on purpose;
this keeps them there. It is also what the reference implementation does, by being
a single event loop.

**An inbound request is admitted in that queue and served concurrently.** Ordered
admission records the handshake, turn, and permission-request state before a
later wire message can act on it; only its handler leaves the queue. That keeps an
agent waiting for a permission answer cancellable without letting scheduler order
replace wire order. `$/cancel_request` goes further and is handled in the read
loop itself, because a cancellation queued behind the work it was meant to stop
is the same as no cancellation.

The consequence for a handler is worth stating plainly, because it is the one
thing this design asks of an application: **a notification handler must not make a
call on the same connection and wait for it.** Its own response would be queued
behind it. Spawning the work instead is what the session handle being valid beyond
the handler call is for.

The queue is unbounded rather than a channel with a size, because a slow handler
must not stall the read loop: the request that would unblock it may be the next
message on the wire.

## Extension methods, with standard names reserved

The v1 schema defines `ExtRequest`, `ExtResponse` and `ExtNotification` in both
directions. Without an escape hatch an ACP extension cannot be implemented
through this package at all, so `Call` and `Notify` exist — but an unrestricted
string is a hole straight through every invariant above. A caller could pass
`session/prompt` and bypass the generated params type, outbound validation,
session-ID binding and the capability gate; a fallback handler could intercept a
future standard method before this package knows it.

**Only names beginning with `_` are extensions.** That is the published
extensibility rule; every other name is reserved for ACP. `Call` and `Notify`
enforce it before writing, fallback handlers see only that namespace, and known
standard methods still have exactly one path through the typed codec and
capability gate. An unknown reserved notification is ignored, while an unknown
reserved call receives method-not-found. If a diagnostic tool ever needs raw
access to a standard method, that is a separately named unsafe API, not a side
effect of ordinary extension support.

## What is generated, and what is exported

Generating all 170 definitions as exported types would contradict the package
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

### The method table is generated; the envelope is not

`meta.json` is the method directory, and the generated table is what the extension
API reserves and what dispatch looks a method up in. Every name in it is
unexported, because a caller of this package names a method by calling the
operation for it.

Neither source is sufficient alone, so both are read. `meta.json` lists the
methods and which peer serves each; it says nothing about whether a method expects
a response. The schema's `x-method` and `x-side` annotations say that — a method
with a `…Response` payload is a request, one with a `…Notification` payload is a
notification — but they are spread across 46 definitions. Generation cross-checks
the two and stops on a disagreement, which is the check
[roadmap.md](./roadmap.md#2-wire) asks CI for, made structural rather than
periodic.

There are **25 distinct method names** in the published stable method table.

**The envelopes are not generated at all**, and the previous revision's
"envelopes and generated routing unions stay unexported" understated it.
`AgentRequest`, `ClientResponse` and their four siblings are JSON-RPC's grammar —
an id, a method name, params — which `internal/jsonrpc2` owns; generating a second
set of types for it would be two sources of truth for one thing. The routing
unions inside them, "every request an agent can send", are of no use either: a
connection dispatches on the method name rather than trying routing-union arms.
The root manifest keeps the envelopes outside the public closure, and a test
holds that boundary against schema changes.

### Some generated types are not exported

The manifest has an `internal` list, and a definition on it is generated with an
unexported name. `RequestId` and `CancelRequestNotification` are on it: a caller
cancels by cancelling a context and never names a request identifier.

The rule that makes this safe is checked at generation time — **no exported type
may name an unexported one**. Such a type compiles and is unusable: an importer
can hold the value but cannot write the field's type, construct one, or switch on
it, and a published type whose field a caller cannot name is worse than no type.
So the internal list has to be closed under what reaches it, and the generator
refuses to emit a tree where it is not.

A generated helper is named after the exported spelling even when its type is not
— `unmarshalRequestID` for `requestID` — because a lower-cased type name spliced
into a helper's name is neither readable nor conventional.

### The capability table is complete before it is needed

Every one of the 25 methods has a row, classified as baseline, gated by a named
predicate, or not implemented yet. It is hand-maintained because it has to be: the
schema has `x-method` and `x-side` and **no annotation linking a method to a
capability**, so those links exist only in prose. Each row quotes the schema's own
words beside the classification, and a test holds the table against the generated
method table in both directions — a method with no row, and a row naming a method
the schema no longer has.

"Not implemented yet" is a classification rather than an omission. The alternative
was a table that covers as far as the work has got, which cannot tell a method
nobody has classified from one deliberately refused.

The third piece design.md promised for each row — the complete handler group
required to advertise it — arrives with the handlers themselves. What exists now is
the fact each group is derived from: `clientCapabilities.terminal` is one boolean
whose row is shared by five methods, and `clientCapabilities.fs` is two booleans
with a row each.

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
- private JSON-RPC plumbing the connection needs, with a reason each definition
  must not be exported;

plus, until the payload roots reach them, the definitions rooted only to exercise
a wire shape — the layer-1 spike's probes, each recording which shape it proves.

The closure's size is an output. The generator writes every exported name it
produces to `schema/exported.txt`, and two gates read it: regenerating in `-check`
mode proves the file matches the manifest, and a test parses the package's own
declarations and holds them against it in both directions. So a generated name
that vanished and a type added by hand to a surface meant to be generated are
both failures, and the hand-written exports are a closed list with a reason
beside each one. The manifest has no compatibility carve-out for experimental
definitions because only the published stable schema is an input.

### Names are the schema's, put through Go's initialism rule

A generated type keeps the schema's name; what changes is capitalisation, and only
because the linter that reads Go's convention insists. `SessionId` becomes
`SessionID` and `HttpHeader` becomes `HTTPHeader`, using **revive's own initialism
list verbatim** rather than one this repository invented: a capitalisation the
linter accepts and a Go programmer does not is the failure mode, and using the
linter's list is what keeps the rule from drifting.

This produced one collision worth recording. The schema defines a
`ProtocolVersion` type, and the package already had a `ProtocolVersion` constant.
The schema's name wins — [AGENTS.md](../AGENTS.md) says the wire grammar is not
ours to design, and that extends to what it calls things — so the constant is
`CurrentProtocolVersion`, typed as the schema's `ProtocolVersion`.

### The doc comments are the specification's prose

A generated doc comment is the schema's `description`, reproduced rather than
rewritten, including its emphasis and status labels. The generator does not
interpret prose as API policy. Two things are added, both punctuation:

- The symbol's name and an em dash, prefixed to the first line, because Go's
  convention and the linter that enforces it both want a doc comment to begin with
  the name it documents. An em dash rather than a rewrite: turning "Content blocks
  represent displayable information" into "ContentBlock is content blocks
  represent displayable information" would be a sentence the specification does
  not contain.
- A full stop, where the last line has none — several descriptions end in a link.

The alternative was switching the comment linters off for the one file whose
comments are most worth checking. A union's first line is generated rather than
quoted, because it has to name the union's arms and the schema's description does
not.

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
- A failed read closes the logical connection. A failed write also closes it
  unless the transport returns exactly `ctx.Err()`, which promises that no part
  of the message was committed. A transport that cannot prove that returns a
  different error even when context cancellation caused the failure.
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
with upstream's copyright and licence headers intact. It is `internal` because it
is an implementation detail, and a fork because the package is `internal` upstream
and cannot be imported.

**The fork is the message layer only**, and that is narrower than the previous
revision said. Upstream is 2,700 lines in nine files, and what it offers beyond
messages is a byte-stream framer, a dialer, a server, a binder and a preempter.
The first of those is where this module's `Transport` already stands: a transport
hands over whole messages, so framing is the transport author's business. Carrying
the rest would have meant a dialer, an idle timeout and a preemption hook that
nothing here uses — the speculative abstraction [AGENTS.md](../AGENTS.md) rules
out — and the connection is written against the message types instead.

What is forked is `messages.go` and `wire.go`: request identifiers, the envelope,
and the result-or-error discrimination. Those are exactly the parts a hand-written
implementation gets wrong first, and they are stable. The small fork delta is in
`internal/jsonrpc2/doc.go`: an indented encoder nothing in a protocol stream
wants was removed, as were upstream's non-standard error sentinels — it uses `-32000` for
"overloaded" and `-32002` for "server is closing", which the Agent Client Protocol
defines as authentication required and resource not found. Two meanings for one
code in one binary is a bug waiting to be found by somebody debugging a live
connection.

Request IDs also keep explicit `null` distinct from an absent ID and normalize an
exact integral JSON number into `int64`. The exact parser accepts equivalent JSON
number spellings such as `1.0` and `1e0`, while rejecting a non-integral value or
an int64 overflow. Upstream's generic JSON-RPC representation decodes through
`float64`, which could lose a large identifier and answer the wrong call. Error
codes are separately checked against the schema's int32 union rather than
truncated or replaced with a code the peer did not send.

Inbound envelopes decode once into `map[string]json.RawMessage`. Exact map lookup
rejects `JSONRPC` where decoding a tagged struct would accept it as `jsonrpc`, and
the raw values preserve member presence before zero values can erase it. Response
and error objects can therefore validate their required, mutually exclusive
members without a second whole-document decode. MCP needs its internal exact-name
decoder because it decodes many tagged structures directly; copying that
dependency here would preserve an implementation detail rather than the design
property ACP actually needs.

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

## Interoperability is recorded, not simulated

Everything else in this module's tests could pass while the wire was wrong, because
both ends would be this implementation. So the evidence is a real subprocess built
on the reference SDK, and what crossed the wire between them is committed.

The arrangement is the fixture corpus's, and so is the reasoning.
`scripts/interop.sh` runs it against a pinned SDK commit and writes the
transcripts; `go test` replays them with no network and no Node. Four scenarios,
each chosen for what it exercises rather than for realism: a turn with a permission
prompt in the middle, because that is the only shape where a peer is caller and
callee at once; a cancelled turn whose final updates still arrive, because that is
the obligation the protocol places on both sides; authentication, because -32000 is
control flow rather than failure; and the workspace methods, because they are every
capability-gated call a client may be asked for.

Two details of the replay are decisions rather than mechanics. **Request
identifiers are mapped, not compared** — this client mints its own, and insisting
they match the recording would be asserting an implementation detail instead of the
protocol. And **the transcript records what the client made of the exchange, not
only the exchange**: the messages prove the two implementations exchanged what they
exchanged, and the observations prove this package drew the right conclusions from
them. A client that read the right bytes and reported the wrong updates fails the
second check.

What replaying gives up: it cannot notice the reference implementation changing.
Re-recording notices that, which is why the updater is pinned and scheduled rather
than run once.

## The published stable schema is the authority

This module vendors the `schema.json` and `meta.json` assets attached to the
`schema-v1.21.0` release. The local TypeScript checkout contains experimental
providers, ACP-carried MCP, document sync, NES, forking and compaction additions;
they are useful implementation references but are not part of those release
assets. Encoding them here would create a local dialect, so they are excluded
until they appear in a published schema release.

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
