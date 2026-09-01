# Changelog

Caller-visible changes, newest first. Entries describe what a user of the module
must do differently, not what happened inside it.

This module is pre-1.0. Until a `v1` tag exists, a minor version may change the
public API; the migration note in each entry is the compatibility promise.

## Unreleased

### Added

The protocol, both halves of it, over any transport that can carry messages in
both directions.

- **The two peers.** `acp.NewClient` and `acp.NewAgent`, each from a `Config`
  whose handler fields are grouped the way the specification's capabilities are —
  `fs` is two independent booleans, `terminal` is one covering five methods.
  Construction fails rather than accept an advertisement the handlers cannot
  serve, or a configuration missing a baseline handler: there is no outcome a
  client may assume for a permission request, so a missing handler for it is a
  refusal at construction rather than an invented answer at runtime.
- **Connections and sessions as separate types.** `Client.Connect` performs the
  handshake and returns a connection that is already initialized; `Agent.Connect`
  accepts one, and refuses every other method until it arrives. Its own `Call` and
  `Notify` wait for the handshake rather than failing on it, so `ctx` is the
  caller's answer to "how long": before it, nobody has agreed what the connection
  can carry, and after it a handler is running only because the client already has
  the answer. A `ClientSession`
  drives turns and an `AgentSession` serves them — a client never calls
  `RequestPermission` and an agent never calls `Prompt`, so one type carrying both
  would make those calls compile and fail at runtime.
- **A turn**: `session/new`, `session/load`, `session/prompt`, `session/update`,
  `session/request_permission`, `session/set_mode`, `authenticate`, and the two
  cancellations. `errors.Is(err, acp.ErrAuthRequired)` is how a client recognises
  "authenticate first", which is control flow rather than failure.
- **The workspace**: `fs/read_text_file`, `fs/write_text_file`, and the five
  terminal methods behind a `TerminalHandle` that binds both identifiers they
  need.
- **Transports**: `acp.NewInMemoryTransports` for two peers in one process,
  `acp.NewStdioTransport` and `acp.NewCommandTransport(*acp.CommandConfig)` for a
  local subprocess, and `acp.NewIOTransport` for any closeable stream pair. A
  custom transport implements `acp.Transport`; the concurrency and shutdown
  contract it is being asked to promise is in the interface's own documentation.
- **The session lifecycle.** `logout`, `session/list`, `session/delete`,
  `session/resume`, `session/close` and `session/set_config_option`, each gated on
  the capability the schema names and advertised by setting its handler.
  `ClientConn` gains `Logout`, `ListSessions`, `DeleteSession` and
  `ResumeSession`; `ClientSession` gains `Close` and `SetConfigOption`.
  `AgentConfig` gains the matching handlers. Closing a session cancels the work
  still running in it before the handler frees anything, because the schema makes
  that the agent's obligation rather than the application's — an outstanding
  `Prompt` still answers with the cancelled stop reason. A closed or deleted
  session is also forgotten, which is the only way the handle population shrinks.
  `elicitation/*` is still not served, and the README says so.
- **Bounded appetite.** A message's size and the time this side will wait were
  already bounded; its count was not, and count is what a peer controls for free.
  A connection now holds at most 1024 messages read but not yet delivered, 1024
  inbound calls at once, and 1024 session handles — the last because a handle
  keeps the one-prompt-at-a-time rule and so can never be reclaimed. Breaching one
  ends the connection with an error that says which. It is not backpressure:
  refusing to read until the backlog drains would turn the documented rule that a
  notification handler must not wait on its own connection into a deadlock,
  because the response that would release it arrives on the read loop that is no
  longer reading.
- **Bounded ownership of a subprocess agent.** Closing a command connection closes
  the pipes, then waits, asks the process to stop, waits again, and finally kills
  it. Each step has `CommandConfig.TerminationGrace` as its deadline, five seconds
  by default, so an agent that ignores end of input cannot hold the client that
  started it. The agent's exit status is not the connection's error: an agent that
  exits non-zero after being asked to stop has stopped, not failed. What *is* the
  connection's error is a failure to release it, which `Close` and `Wait` both
  report: a subprocess that could not be reaped is still running, and answering
  `nil` would be reporting it as gone.
- **Nothing this side sends can precede the initialize response.** An agent's
  outbound operations wait on the response having been written. A flag set after
  that write is inherently late — the client observes the answer *during* the
  write, and the request it sends next is served on a different goroutine from the
  one still finishing the handshake — so a flag refused handlers that were only
  running because the handshake had already succeeded.
- **A rejected initialize does not poison the connection.** Invalid parameters
  establish no capability agreement, so after the error response settles the
  next queued or later request may negotiate. Attempts are serialized in wire
  order, and every attempt ordered after an accepted one remains an invalid
  renegotiation.
- **Terminal handles are spent once released.** `TerminalHandle.Release` documented
  that the handle was unusable afterwards; now it is. Every operation on a released
  handle, including a second `Release`, returns `acp.ErrTerminalReleased`. A
  released identifier is the client's to reuse, so a stale handle could otherwise
  name a terminal that had come to belong to something else.
- **The negotiated handshake, whole.** `Peer()` returns the protocol version, both
  halves of the capabilities, both peers' `Info` and `_meta`, and the agent's
  `AuthMethods` — which is where a client finds the `methodId` to authenticate
  with rather than knowing one out of band. It is a deep copy, all the way down
  through the twenty-odd `_meta` maps the capability tree nests: the same value
  backs the capability gate, and a caller who could mutate it could widen its own
  authority. The configuration a client or agent is built from is copied the same
  way, so changing it afterwards changes nothing that was already validated.
- **An owned `_meta` boundary.** `Meta` encodes extension values when they are set
  and stores only JSON. This prevents an arbitrary Go object with hidden mutable
  state from leaking through a configuration or `Peer()` snapshot. Construct one
  with `NewMeta`, change it with `Set`, and read a typed value with `Decode`.
- **A handshake built for the peer in front of it.** One field of the initialize
  response depends on what this client offered, and the schema says so. A terminal
  authentication method is advertised only to a client that enabled
  `clientCapabilities.auth.terminal`.
- **`authenticate` names a method the handshake advertised.** The schema says the
  identifier "must be one of the methods advertised in the initialize response",
  and that a terminal method must not be passed to `authenticate` at all — it is
  performed by running the agent in a terminal. Both sides hold to it, so a peer
  that does not go through this package cannot reach the handler with an
  identifier the agent never offered. `NewAgent` requires an `Authenticate`
  handler only for the methods it would actually serve, and refuses two methods
  that share an identifier, which the schema calls unique and which a client
  selects by.
- **The extension boundary**: `Call`, `Notify` and fallback handlers, in both
  directions, for `_`-prefixed methods the specification reserves for extensions.
  Every other name is reserved for ACP and refused there, so a private fallback
  cannot claim a future standard method before the typed codec and capability gate
  know it.
- **The complete capability table.** All 25 methods are classified as baseline,
  gated by a named predicate, or not implemented yet, with the schema's own words
  quoted beside each. A test holds it against the generated method table in both
  directions, construction refuses an advertisement whose method this package does
  not serve, and both directions consult the gate.
- **The envelope is checked before anything is done about it.** A standard method
  used as the other kind is refused rather than served: a notification method sent
  as a call is answered "invalid request", and a request method sent as a
  notification is dropped, because `terminal/kill` sent that way would kill a
  terminal and answer nobody. A response must carry a result or an error and
  exactly one of them; its error must carry the schema's exact `code` and
  `message` members. JSON member names are matched case-sensitively, malformed
  and trailing values are rejected, and an empty object remains a result
  while a missing one is not. Request IDs preserve the schema's absent,
  explicit-null, string and int64 states;
  non-integral and overflowing numbers are rejected rather than silently
  coerced, while JSON number spellings such as `1.0` are accepted when their
  exact value is an int64. Reusing an ID while its first request is active ends
  the connection instead of overwriting that request's cancellation and answer
  ownership.
- **The generated protocol types**, from `schema/schema.json` at upstream release
  `schema-v1.21.0`. Generation now reaches every operation the API implements —
  142 of the published schema's 170 definitions. JSON-RPC envelopes are
  deliberately not generated: `internal/jsonrpc2` owns JSON-RPC's grammar, and a
  second set of types for it would be two sources of truth.

- **Examples that run.** Every example in the package documentation is a working
  program with both peers in one process over `acp.NewInMemoryTransports`, so
  `go test` fails when one goes stale: a turn, a cancellation and the stop reason
  it obliges, authentication, a terminal handle's life, extension methods, `Opt`
  and `Meta`. `examples/` has an agent binary and a client that spawns it, which
  is the same code over a real pipe between two processes.

### Changed

- The vendored v1.21.0 files now come directly from the published release assets.
  Experimental additions found only in the local TypeScript SDK checkout —
  providers, ACP-carried MCP, document sync, NES, session forking, compaction and
  position encoding — were removed from the generated Go API. Migration: use
  extension methods for private experiments, or wait for those shapes to enter a
  published ACP schema.
- `Meta` is no longer `map[string]any`. Migration: replace map literals with
  `acp.NewMeta`, use `Set` for updates, and use `Decode` to read a value.
- `jsonrpc.Response.Error` is now `*jsonrpc.Error`, the only error shape a
  JSON-RPC response can carry. Migration: custom transports construct the
  structured wire error directly instead of assigning an arbitrary Go error.

### How this is known to work

- **125 cross-SDK fixtures**, whose expected outcomes come from the TypeScript
  SDK's deserialisation machinery regenerated from the pinned published schema.
  The updater runs in an isolated archive, so neither the local reference
  checkout nor its unstable v1 schema can change the oracle. `go test` replays
  the results with no network and no Node.
- **Four recorded interoperability transcripts**, produced by running this module's
  client against an agent built on the reference SDK as a real subprocess: a turn
  with a permission prompt, a cancelled turn whose final updates still arrive,
  authentication, and every capability-gated workspace method.
  `scripts/interop.sh` records them and `go test` replays them.
- **Two fuzz targets** asserting that normalisation is a fixed point, which is the
  property schema-directed recovery can quietly break.
- **Cancellation and shutdown tested with `testing/synctest`** rather than with
  sleeps.

### Three contracts worth reading before writing a handler

- **A notification handler must not make a call on the same connection and wait
  for it.** Notifications are served in arrival order and a response is delivered
  only after every notification that arrived before it — which is what stops a
  turn's last chunk arriving after the turn ends — so a handler that waited for its
  own response would wait for itself. Spawn the work instead; the session handle is
  valid beyond the handler call for exactly that.
- **One prompt at a time per session, and a turn outlives the caller waiting on
  it.** `session/cancel` carries only a session identifier — no turn, no request —
  so a session with two turns would have no way to say which one a cancellation
  meant. A second overlapping prompt returns `acp.ErrPromptInProgress`, and a
  second one arriving from any peer is refused on the wire for the same reason.
  Cancelling a `Prompt` context stops *that caller* waiting; it does not end the
  turn, so the session stays claimed until the agent's answer arrives, and the
  connection keeps the pending call only to observe that transition. `Cancel` is
  the operation that ends a turn,
  and it holds the session until its notification is on the wire: `session/cancel`
  names only a session, so a prompt that started before it went out would be the
  turn the agent applies it to.
- **Nothing is served before the handshake, and nothing after it is refused.** A
  client refuses every inbound method — including extension fallbacks — that
  reached it before the initialize answer did, and serves everything the agent was
  entitled to send afterwards. Those are two facts: the read loop saw the order,
  and the answer is validated and published from the delivery queue so that the
  order survives. An agent refuses everything but `initialize` before one arrives,
  and waits before *sending* anything until it has written the answer, so nothing
  it sends can overtake the response that told the client what it agreed to.
- **This package speaks protocol version 1 and no other.** An agent answers
  `initialize` with the version it implements, which is what the schema asks for —
  the version the client requested if it is supported, and its own latest
  otherwise. A client whose peer answers anything else disconnects rather than
  speak a grammar it does not have. A protocol number identifies a grammar, not a
  feature level whose minimum is automatically safe.

### Migration

Nothing to migrate from: no version has been released.
