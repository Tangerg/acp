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
- **A handshake built for the peer in front of it.** Two fields of the initialize
  response depend on what this client offered, and the schema says so. A terminal
  authentication method is advertised only to a client that enabled
  `clientCapabilities.auth.terminal`; the agent's `positionEncoding` is chosen from
  the encodings the client offered, or omitted. A client refuses an encoding it
  never offered rather than proceed under two readings of the same offsets, and
  refuses to pass a terminal method to `authenticate` — the schema says it must
  not, because that method is performed by running the agent in a terminal.
  `NewAgent` correspondingly requires an `Authenticate` handler only for the
  methods it would actually serve.
- **The extension boundary**: `Call`, `Notify` and fallback handlers, in both
  directions, for methods the specification does not define. A method it does
  define is refused there, because it has exactly one path through the typed codec
  and the capability gate.
- **The complete capability table.** All 42 methods are classified as baseline,
  gated by a named predicate, or not implemented yet, with the schema's own words
  quoted beside each. A test holds it against the generated method table in both
  directions, construction refuses an advertisement whose method this package does
  not serve, and both directions consult the gate.
- **The envelope is checked before anything is done about it.** A standard method
  used as the other kind is refused rather than served: a notification method sent
  as a call is answered "invalid request", and a request method sent as a
  notification is dropped, because `terminal/kill` sent that way would kill a
  terminal and answer nobody. A response must carry a result or an error and
  exactly one of them; an empty object is a result, a missing one is not.
- **The generated protocol types**, from `schema/schema.json` at upstream release
  `schema-v1.21.0`. Generation now reaches every operation the API implements —
  167 of the schema's 265 definitions — and the generator handles all but the six
  JSON-RPC envelopes, which are deliberately not generated: `internal/jsonrpc2`
  owns JSON-RPC's grammar, and a second set of types for it would be two sources
  of truth.

### How this is known to work

- **125 cross-SDK fixtures**, whose expected outcomes come from the reference
  implementation's own validators rather than from this repository's reading of the
  schema. `scripts/update-fixtures.sh` produces them from a pinned SDK commit; `go
  test` replays them with no network and no Node.
- **Four recorded interoperability transcripts**, produced by running this module's
  client against an agent built on the reference SDK as a real subprocess: a turn
  with a permission prompt, a cancelled turn whose final updates still arrive,
  authentication, and every capability-gated workspace method.
  `scripts/interop.sh` records them and `go test` replays them.
- **Two fuzz targets** asserting that normalisation is a fixed point, which is the
  property schema-directed recovery can quietly break.
- **Cancellation and shutdown tested with `testing/synctest`** rather than with
  sleeps.

### Two contracts worth reading before writing a handler

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
  turn, so the session stays claimed until the agent's answer arrives, and a
  waiter stays behind to observe it. `Cancel` is the operation that ends a turn.
- **Nothing is served before the handshake, on either side.** A client refuses
  every inbound method — including extension fallbacks — until it has accepted the
  initialize answer, because until then there is no negotiated peer to judge it
  against. An agent refuses everything but `initialize` before one arrives, and
  refuses to *send* anything until it has written the answer, so nothing it sends
  can overtake the response that told the client what it agreed to.
- **This package speaks protocol version 1 and no other.** An agent answers
  `initialize` with the version it implements, which is what the schema asks for —
  the version the client requested if it is supported, and its own latest
  otherwise. A client whose peer answers anything else disconnects rather than
  speak a grammar it does not have. A protocol number identifies a grammar, not a
  feature level whose minimum is automatically safe.

### Migration

Nothing to migrate from: no version has been released.
