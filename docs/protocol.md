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

Both directions carry requests. This is not a client calling a server: 14 of the
43 methods run from the agent to the client.

## Methods

Counted from `schema/meta.json` at schema release `schema-v1.21.0`: **28 agent
methods, 14 client methods, and one protocol method — 43 in total.** The
`protocolVersion` that `initialize` negotiates is `1`.

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
| | `session/resume` | Resume without replaying history. |
| | `session/list`, `session/delete`, `session/fork`, `session/close` | Session management, each capability-gated. |
| | `session/set_mode`, `session/set_config_option` | Switch agent mode or a config option mid-session. |
| Providers | `providers/list`, `providers/set`, `providers/disable` | Model provider selection. |
| Editor state | `document/didOpen`, `document/didChange`, `document/didClose`, `document/didSave`, `document/didFocus` | LSP-shaped document sync, so the agent knows what the user is looking at. |
| Next edit | `nes/start`, `nes/suggest`, `nes/accept`, `nes/reject`, `nes/close` | Inline next-edit suggestions. |
| MCP | `mcp/message` | Pass an MCP message through to a server the agent holds. |

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
| MCP | `mcp/connect`, `mcp/message`, `mcp/disconnect` | The client holds the MCP connection on the agent's behalf. |

### Protocol

`$/cancel_request` cancels a single in-flight JSON-RPC request. LSP's spelling,
not MCP's `notifications/cancelled`. It is distinct from `session/cancel`, which
cancels a *turn* — see [design.md](./design.md#two-cancellations-kept-apart), where
the difference decides an API signature.

## Capability gating

`initialize` is a capability exchange. Many methods may only be called if the
peer advertised the capability that gates them: an agent that was never told the
client can read files does not ask it to.

Not everything past the baseline is gated, and it is worth being precise because
an earlier draft of these notes was not. `session/cancel` and `session/update`
are baseline session operations with no capability behind them, and there are
other exceptions. The gate for any given method is whatever the schema's
capability types say, which is the only list worth trusting.

Where a capability does exist, it is not always one per method.
`ClientCapabilities.terminal` is a single boolean documented as "Whether the
Client support all `terminal*` methods" — one flag for five methods — while
`ClientCapabilities.fs` is two independent booleans, `readTextFile` and
`writeTextFile`.

This makes capability advertisement a correctness concern rather than metadata:
capabilities decide whether an agent may read a file or run a command, so they
are an authority boundary. It is why
[design.md](./design.md#handlers-are-fields-and-capabilities-are-derived-from-complete-groups)
derives what is advertised from what is implemented, in whatever grouping the
capability type actually has.

## Errors

`ErrorCode` names eight values: the six JSON-RPC standard codes plus two ACP
codes in the reserved implementation range.

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

Two are control flow rather than failure. **`-32000`** is how an agent answers
`session/new` to say the client must call `authenticate` first — a documented
step in the lifecycle, not a fault. **`-32800`** is what a peer returns for a
request killed by `$/cancel_request`, so it is the far side of a caller's own
cancellation. [design.md](./design.md#errors) maps both to Go accordingly.

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
can answer it. [design.md](./design.md#two-cancellations-kept-apart) keeps them
as two separate operations in Go for this reason.

## The type system

From `schema/schema.json` at `schema-v1.21.0`: **265 definitions and 41 unions.**

The unions do not split in two. They split in five, and the schema is explicit
about which is which:

| Class | Count | Open? |
| --- | --- | --- |
| Closed string enumerations — `StopReason`, `ToolKind`, `Role` | 14 | No |
| Open string unions — const arms plus a bare `string`: `LlmProtocol`, `CompactionStatus`, `SessionConfigOptionCategory` | 3 | Yes |
| Closed discriminated object unions — `SessionUpdate`, `ContentBlock` | 16 | No |
| Open object unions carrying a `not` catch-all | 4 | Yes |
| Primitive, value and mixed — `RequestId`, `ErrorCode`, `ElicitationContentValue` | 4 | n/a |

The largest is `SessionUpdate`, with **15 arms** tagged by a `sessionUpdate`
field. It is also the one sent most: it is the whole of a turn's output. It is
**closed**.

Openness is a property the schema states, not one an implementation may grant
itself. `zStopReason` is five literals with no fallback; `zLlmProtocol` is five
literals plus `z.string()` (`src/schema/zod.gen.ts:2075`, `:1671`). Exactly four
unions carry the `not` clause that makes an object union open.

Two schema extension keywords also carry decoding semantics no plain JSON decoder
implements: **`x-deserialize-default-on-error` appears 378 times** and
**`x-deserialize-skip-invalid-items` 35 times**. A malformed value under the
first becomes the schema's declared default rather than failing the message.

`schema.json` also marks **52 of its 265 definitions UNSTABLE** — providers,
ACP-carried MCP, document sync, NES, session forking, compaction — so being in
the v1 lane is not the same as being stable.

[design.md](./design.md#unions) treats all of this as the governing constraint
rather than detail.
