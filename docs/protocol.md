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
cancels a *turn* — see [design.md](./design.md#cancellation-is-not-ctxerr), where
the difference decides an API signature.

## Capability gating

`initialize` is a capability exchange. Past the baseline — `initialize`,
`authenticate`, `session/new`, `session/prompt` on one side and
`session/request_permission` on the other — a method may only be called if the
peer advertised the capability that gates it. An agent that was never told the
client can read files does not ask it to.

This makes capability advertisement a correctness concern rather than metadata,
and it is why [design.md](./design.md#handlers-are-fields-and-they-imply-capabilities)
derives what is advertised from what is implemented instead of asking a caller to
keep two lists in step.

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

## The type system

From `schema/schema.json` at `schema-v1.21.0`: **265 definitions and 41 unions.**

The unions split cleanly in two, and the split decides how they are represented
in Go:

- **14 are closed sets of strings** — `StopReason`, `ToolKind`, `ToolCallStatus`,
  `Role`, `PlanEntryPriority` and friends.
- **27 carry a discriminator and a payload.** The largest is `SessionUpdate`,
  with **15 arms** tagged by a `sessionUpdate` field. It is also the one sent
  most: it is the whole of a turn's output.

Both counts move upward over time. `SessionUpdate` did not start at fifteen.
[design.md](./design.md#unions) treats that as the governing fact rather than a
detail.
