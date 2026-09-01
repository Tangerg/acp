# The protocol

What the Agent Client Protocol is, as a set of facts to implement against. This
page describes upstream, not this repository; where the two disagree, upstream is
right. Sources are listed in [README.md](./README.md#sources).

## Roles

A **client** is a code editor. It owns the workspace: the files, the terminals,
the user, and the authority to say yes to anything that touches them.

An **agent** is a program that uses a model to read and change that workspace.

The point of the protocol is that neither has to know which implementation is on
the other end. From
[the introduction](https://agentclientprotocol.com/get-started/introduction), the
problem it solves is that "every new agent-editor combination requires custom
work" — an N×M problem the protocol turns into N+M.

ACP borrows JSON representations from MCP where they fit, and adds its own types
for the parts of an agentic interface MCP has no opinion about — notably diffs.
Markdown is the default for human-readable text: rich enough to be worth
rendering, plain enough that a terminal client can ignore the markup.

## Transport

One bidirectional JSON-RPC 2.0 connection. Locally, the client starts the agent
as a subprocess and speaks newline-delimited JSON over its stdin and stdout.
Remote agents over HTTP or WebSocket are described upstream as work in progress.

Both directions carry requests. This is not a client calling a server: 11 of the
25 methods run from the agent to the client.

## Methods

Counted from the published `schema/meta.json` asset at schema release
`schema-v1.21.0`: **13 agent methods, 11 client methods, and one protocol method —
25 distinct method names.**

The `protocolVersion` that `initialize` negotiates is `1`.

### Client → agent

| Area | Method | Notes |
| --- | --- | --- |
| Lifecycle | `initialize` | Negotiate version and exchange capabilities. Always first. |
| | `authenticate` | Only if the agent asks for it. |
| | `logout` | Gated on `agentCapabilities.auth.logout`. |
| Session | `session/new` | Create a conversation. Returns a `sessionId`. |
| | `session/prompt` | Run one turn. Blocks until the turn ends. |
| | `session/cancel` | Notification. Cancels the current turn. |
| | `session/load` | Gated on `loadSession`. Replays history as notifications. |
| | `session/list`, `session/delete`, `session/resume`, `session/close` | Session management, each capability-gated. |
| | `session/set_mode`, `session/set_config_option` | Switch agent mode or a config option mid-session. |

### Agent → client

| Area | Method | Notes |
| --- | --- | --- |
| Turn output | `session/update` | Notification. The agent's running commentary for a turn: message chunks, thoughts, tool calls, plans. |
| Authority | `session/request_permission` | Ask the user to approve a tool call. Baseline — every client implements it. |
| Filesystem | `fs/read_text_file` | Gated on `fs.readTextFile`. |
| | `fs/write_text_file` | Gated on `fs.writeTextFile`. |
| Terminals | `terminal/create`, `terminal/output`, `terminal/wait_for_exit`, `terminal/kill`, `terminal/release` | Gated on the `terminal` capability. |
| Elicitation | `elicitation/create` | Ask the user for structured input. |
| | `elicitation/complete` | Notification, reporting that an out-of-band interaction finished. |

### Protocol

`$/cancel_request` cancels a single in-flight JSON-RPC request. LSP's spelling,
not MCP's `notifications/cancelled`. It is distinct from `session/cancel`, which
cancels a *turn* — see [design.md](./design.md#two-cancellations-and-who-owns-each), where
the difference decides an API signature.

## Capability gating

`initialize` is a capability exchange. Many methods may only be called if the
peer advertised the capability that gates them: an agent that was never told the
client can read files does not ask it to.

Not everything past the baseline is gated, and it is worth being precise because
an earlier draft of these notes was not. `session/cancel` and `session/update`
are baseline session operations with no capability behind them, and there are
other exceptions.

Which method is gated by which capability is **not machine-readable**. The schema
annotates payloads with `x-method` and `x-side`, 46 occurrences each, and has no
annotation linking a method to a capability predicate — those links live only in
prose descriptions. Any implementation therefore maintains that mapping itself;
[design.md](./design.md#the-capability-table-is-hand-maintained-and-checked)
makes it a pinned table that CI checks against `meta.json` rather than something
read off the schema at runtime.

Where a capability does exist, it is not always one per method.
`ClientCapabilities.terminal` is a single boolean documented as "Whether the
Client support all `terminal*` methods" — one flag for five methods — while
`ClientCapabilities.fs` is two independent booleans, `readTextFile` and
`writeTextFile`.

This makes capability advertisement a correctness concern rather than metadata:
capabilities decide whether an agent may read a file or run a command, so they
are an authority boundary. It is why
[design.md](./design.md#handlers-capability-groups-and-a-checked-table)
derives what is advertised from what is implemented, in whatever grouping the
capability type actually has.

## Errors

`ErrorCode` names eight **predefined constants** — the six JSON-RPC standard
codes plus two ACP codes in the reserved implementation range — and then a ninth
arm titled *Other*, an unrestricted `int32`. It is an open integer union, so an
unknown in-range code is valid and must survive decoding. The TypeScript type is
the eight literals plus `number` (`src/schema/types.gen.ts:3605`).

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

**`-32000`** is control flow rather than failure: it is how an agent answers
`session/new` to say the client must call `authenticate` first, a documented step
in the lifecycle.

**`-32800`** is subtler than it looks. The schema says execution "was aborted
either due to a cancellation request from the caller or because of resource
constraints or shutdown" — so receiving it does not prove the receiver cancelled
anything. [design.md](./design.md#errors) keeps local context completion and
remote `-32800` distinct for that reason.

## The shape of a turn

1. `initialize` — versions and capabilities.
2. `authenticate`, if the agent asked for it.
3. `session/new` (or `session/load`) — a `sessionId`.
4. `session/prompt` — the client sends the user's message and waits.
5. While that request is outstanding, the agent streams `session/update`
   notifications and may call back into the client: `session/request_permission`
   before a sensitive tool call, `fs/*` and `terminal/*` to do the work.
6. The turn ends when the agent answers the original `session/prompt` with a
   `StopReason`.

Step 5 is why both peers serve requests. An agent that is answering a prompt is
simultaneously a caller.

The schema does not require `session/request_permission` to occur inside a prompt
turn. A request received during an active turn is owned by that turn; one received
outside a turn is still dispatched normally, but `session/cancel` has no turn
through which to claim it. Its own `$/cancel_request` remains effective.

### Cancellation

`session/cancel` is a notification, and sending it does not end the turn. The
protocol requires the agent to answer the outstanding `session/prompt` with
`StopReason` `cancelled`, and requires the client to keep accepting
`session/update` until it does — the agent may still have final tool-call
updates to report. A pending `session/request_permission` must be answered
`cancelled` rather than dropped.

The reference implementation states this on both sides
(`acp-typescript-sdk/src/acp.ts:3723`, on `Client.requestPermission`: "If the
client cancels the prompt turn via `session/cancel`, it MUST respond to this
request with `RequestPermissionOutcome::Cancelled`"; and on `Client.sessionUpdate`:
"Clients SHOULD continue accepting tool call updates even after sending a
`session/cancel` notification").

`session/cancel` is not `$/cancel_request`, and the difference is not cosmetic.
`$/cancel_request` cancels one JSON-RPC request — the TypeScript SDK sends it
when a per-request abort signal fires (`src/jsonrpc.ts:99`). `session/cancel`
cancels a turn and leaves the request outstanding, on purpose, so that the agent
can answer it. [design.md](./design.md#two-cancellations-and-who-owns-each) keeps them
as two separate operations in Go for this reason.

## The type system

From the published `schema/schema.json` asset at `schema-v1.21.0`: **170
definitions and 32 unions.**

The schema uses closed enumerations, open string unions, discriminated object
unions, and primitive/value unions. Openness belongs to each definition; it is
not a blanket forward-compatibility policy.

The largest is `SessionUpdate`, with **11 arms** tagged by a `sessionUpdate`
field. It is also the one sent most: it is the whole of a turn's output. It is
**closed**.

Openness is a property the schema states, not one an implementation may grant
itself. `StopReason` is five literals with no fallback, while
`SessionConfigOptionCategory` includes a bare string arm.

Two schema extension keywords also carry decoding semantics no plain JSON decoder
implements: **`x-deserialize-default-on-error` appears 249 times** and
**`x-deserialize-skip-invalid-items` 27 times**. A malformed value under the
first becomes the schema's declared default rather than failing the message.

[design.md](./design.md#unions) treats all of this as the governing constraint
rather than detail.

### Extensions

Custom method names begin with `_`; every other method namespace is reserved for
ACP. Unknown custom requests receive a JSON-RPC response, unknown custom
notifications may be ignored, and extension data belongs under `_meta` rather
than as new root properties. This keeps a private extension from colliding with a
later standard method.
