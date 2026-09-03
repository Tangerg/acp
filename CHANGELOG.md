# Changelog

This file records caller-visible changes, newest first. The module is pre-1.0, so
a minor release may change its public API. Each released entry will include
migration instructions.

## Unreleased

### Added

- **Elicitation**: `elicitation/create` and `elicitation/complete` are served, so
  the SDK now covers all 25 methods in the pinned schema.

  A client sets `ClientConfig.Elicitation` to an `ElicitationHandlers` whose
  `Form` and `URL` fields each advertise the mode they serve. Both modes arrive
  through one method, so a mode with no handler is refused with invalid-params
  rather than method-not-found. `Complete` is required alongside `URL` and
  refused without it.

  An agent calls `AgentSession.CreateElicitation` to elicit within a session, or
  `AgentConn.CreateElicitation` to elicit within the request it is serving.
  The scope is the operation's and is never named by the caller —
  `ElicitationRequestScope` is not exported, because it holds a JSON-RPC request
  identifier and this API does not surface those.

  A client that advertised elicitation before this release was refused at
  construction; it is now accepted when the handlers back it.

- **`Limits`**: `ClientConfig.Limits` and `AgentConfig.Limits` bound what one
  connection will hold on a peer's behalf — the delivery backlog, the inbound
  calls in flight, and the cached session handles. A zero field takes the default
  it had as a constant (1024), so existing configurations are unchanged. A
  negative one is refused by `NewClient` and `NewAgent` rather than by the message
  that would breach it.

  The bound worth setting is `QueuedDeliveries`. It is the only one an honest peer
  reaches: a turn's `session/update` stream is produced by an agent and consumed
  by a `SessionUpdate` handler that may render it, and because a breach ends the
  connection, an application whose handler is slow now has somewhere to say so.

### Fixed

- **A map whose values are a union did not survive the wire.** The discriminant
  belongs to the union rather than to the arm, and such a map was going to
  `encoding/json`: it encoded without the discriminant and could not be decoded at
  all. The schema's only such property is `ElicitationSchema.properties`, so
  nothing reached it until elicitation was served.

### Changed

- **Breaking: five `AgentConfig` handlers take the connection.** `NewSession`,
  `Authenticate`, `Logout`, `ListSessions` and `DeleteSession` now take an
  `*AgentConn` between the context and the request, matching `Prompt` and `Cancel`,
  which have always taken an `*AgentSession`.

  A handler is given the handle its method is scoped to. Without it a
  connection-scoped handler could make no outbound call at all, which put the
  specification's own example of a request-scoped elicitation — an agent asking
  the user for something during authentication — out of reach.

  Migration is mechanical: add a parameter.

  ```go
  // before
  NewSession: func(ctx context.Context, request *acp.NewSessionRequest) (*acp.NewSessionResponse, error)
  // after
  NewSession: func(ctx context.Context, conn *acp.AgentConn, request *acp.NewSessionRequest) (*acp.NewSessionResponse, error)
  ```

  The extension fallbacks and every `ClientConfig` handler are unchanged.

- `Opt` documents that two codecs decode it and how they differ. A generated field
  goes through the schema-directed codec, which leaves an unreadable optional
  property absent so the rest of a peer's message still arrives; an `Opt` in a
  caller's own type goes through `Opt.UnmarshalJSON`, which reports the failure the
  way `encoding/json` does. Behaviour is unchanged; only the difference was
  previously unstated.

## v0.1.0 — 2026-09-02

This is the first release. It implements both peers of Agent Client Protocol
(ACP) version 1 against the published `schema-v1.21.0` assets.

### Added

- **Client and agent APIs**: `NewClient` and `NewAgent` build reusable peers
  from explicit `Config` structs. `ClientConn` and `AgentConn` represent logical
  connections, while `ClientSession`, `AgentSession`, and `TerminalHandle` bind
  protocol identifiers to the connection that owns them.
- **Turns and session lifecycle**: the SDK serves `session/new`, `session/load`,
  `session/resume`, `session/list`, `session/delete`, `session/close`,
  `session/prompt`, `session/cancel`, `session/set_mode`,
  `session/set_config_option`, `session/update`, and
  `session/request_permission`.
- **Authentication**: `authenticate` and `logout` support agent-managed
  authentication. `errors.Is(err, acp.ErrAuthRequired)` identifies the documented
  authenticate-and-retry flow from `session/new`.
- **Workspace operations**: agents can call `fs/read_text_file`,
  `fs/write_text_file`, and all five `terminal/*` methods through session and
  terminal handles.
- **Transports**: `NewCommandTransport` owns an agent subprocess and its bounded
  shutdown. `NewStdioTransport`, `NewIOTransport`, and `NewInMemoryTransports`
  cover stdio, custom streams, and peers in one process. Custom transports
  implement the documented `Transport` and `Connection` contracts.
- **Extension methods**: both peers can call, notify, and handle
  implementation-specific methods whose names begin with `_`. Standard and
  future reserved ACP method names cannot bypass typed dispatch.
- **Wire values**: `Opt` preserves absent, explicit-null, and present JSON states.
  `Meta` owns encoded extension JSON so configuration and `Peer()` snapshots
  remain immutable.
- **Protocol errors**: generated `Error` and `ErrorCode` types integrate with
  `errors.Is` and `errors.As`. `ErrRequestCancelled`, `ErrConnectionClosed`,
  `ErrPromptInProgress`, and `ErrTerminalReleased` cover local control flow and
  lifecycle boundaries.
- **Generated protocol surface**: `schema.gen.go` contains the transitive public
  closure for every implemented operation, currently 142 of 170 definitions.
  Generation uses committed release assets and runs without network access.
- **Resource limits**: each connection bounds message size, queued delivery
  count, inbound calls, session handles, and protocol-owned write waits.
  Exceeding a count limit ends the connection instead of silently dropping
  protocol state.
- **Runnable examples**: package examples cover turns, cancellation,
  authentication, terminals, extensions, `Opt`, and `Meta`. The `examples/`
  programs run the same client and agent over a subprocess pipe.

### Behavioural guarantees

- **Capability enforcement**: optional handlers derive advertisements. Explicit
  capability structs replace the derived advertisement and fail construction
  when they claim an unsupported method. Both inbound and outbound calls enforce
  the negotiated capability. Three capabilities gate parameters rather than
  methods, and the client checks them before sending: prompt content against
  `promptCapabilities`, `http` and `sse` MCP servers against `mcpCapabilities`,
  and `additionalDirectories` against `sessionCapabilities`.
- **Absolute paths**: the protocol requires every path it carries to be absolute,
  and the SDK refuses to send a relative one from a workspace call or a session
  setup. It accepts POSIX and Windows forms, because the path describes the
  peer's filesystem rather than the sending process's.
- **Handshake ordering**: `Client.Connect` returns only after a validated
  handshake. `Agent.Connect` starts reading before `initialize`, but its outbound
  calls wait until the response is written. Invalid initialize parameters do not
  poison the connection; attempts remain serialised in wire order.
- **Turn cancellation**: cancelling a prompt context stops the local caller
  waiting and sends `$/cancel_request`. `ClientSession.Cancel` instead ends the
  turn, resolves pending permission requests as cancelled, and requires the agent
  to answer the prompt with `StopReasonCancelled`.
- **Ordered notifications**: notification handlers retain wire order, and a
  response is delivered after preceding notifications. A notification handler
  must therefore not call the same connection and wait synchronously.
- **Session shutdown**: successful `session/close` cancels active work before the
  handler frees resources, and removes the connection's cached session handle. A
  client also drops its cached handle when `DeleteSession` succeeds.
- **Terminal release**: after a release request reaches the transport, every
  operation on that handle returns `ErrTerminalReleased`. A local refusal before
  transport delivery rolls the claim back.
- **Exact JSON-RPC envelopes**: member names are case-sensitive. Responses require
  exactly one of `result` or `error`; request identifiers preserve absent, null,
  string, and exact int64 values. Reusing an active request identifier ends the
  connection.
- **Error privacy**: only an explicit ACP `Error` reaches the peer. Other handler
  errors are logged through the configured logger and become `-32603 Internal
  error` on the wire.

### Changed before first release

- The vendored v1.21.0 schema now comes from the published release assets.
  Experimental provider, ACP-carried MCP, document sync, NES, session fork,
  compaction, and position-encoding definitions from a repository checkout are
  not part of the generated API.
- `Meta` is an owned JSON value rather than `map[string]any`. Use `NewMeta`,
  `Set`, `Decode`, and `Delete`.
- `jsonrpc.Response.Error` is `*jsonrpc.Error`, the only structured error shape a
  JSON-RPC response can carry.

### Verification

- 125 cross-SDK fixtures use TypeScript SDK validators generated from the pinned
  schema.
- Four subprocess transcripts run this client against an agent built with the
  reference TypeScript SDK.
- A recorded Zed 1.17.2 session exercises the agent side through a prompt, a
  command run through the editor's terminal, and cancellation from its stop
  button.
- Two fuzz targets require wire normalisation to reach a fixed point.
- Concurrency and shutdown run under `testing/synctest`, the race detector, and
  the Linux, macOS, and Windows build matrix.

### Migration

Nothing to migrate from: this is the first release.
