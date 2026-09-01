# Go implementation review, round 1

Review of the first implementation built on the fourth design revision.

**Status: not ready to merge or release.** The architecture can continue and
does not need to be replaced, but seven high-priority correctness issues remain
in connection shutdown, cancellation, capability enforcement, initialization,
method-shape validation, and subprocess ownership.

## Review basis

This pass reviewed the working tree on top of
`89e096bc9c717ae7b428bedb5eae94e3f5a783a2` on 2026-09-01. The implementation
was not committed at the time of review, so that commit identifies the design
baseline rather than an immutable implementation snapshot.

The implementation was checked against:

- the pinned schema in `schema/schema.json`, release `schema-v1.21.0`;
- `~/Desktop/acp-typescript-sdk` at
  `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`, package version 1.4.0;
- `~/Desktop/go-sdk` at
  `21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`;
- the repository rules in [AGENTS.md](../AGENTS.md), especially that the
  published schema owns the wire grammar and advertised capabilities must not
  exceed the implementation.

No code or existing documentation was changed during this review.

## What is working well

The first implementation has several strong foundations that should remain:

- `schemagen` is a narrow slice of the real generator rather than a disposable
  prototype. A single-purpose standard-library `flag` command is appropriate;
  Cobra or Viper would add no value at its current size.
- The schema and method metadata are pinned, generation is reproducible, and
  generated exports are checked against a committed manifest.
- `Opt[T]` gives optional nullable properties distinct absent, null, and value
  states with a useful zero value.
- Required slices, schema-directed fallback, invalid-item recovery, unions,
  constraints, open-union payloads, and stable Go-owned encoding are covered by
  focused runtime and generator tests.
- The TypeScript oracle and real-process interoperability evidence are pinned
  and reproducible without making normal Go tests depend on Node or the network.
- Client and agent sessions have direction-specific handles, handle-bound IDs
  are projected out of public params, and terminal responses retain `_meta`.
- The error type preserves open `int32` codes and keeps local context completion
  distinct from a peer's `-32800` response.
- The public transport package exposes a small but usable JSON-RPC message and
  encode/decode surface.
- Static quality is strong: the code is formatted, lint-clean, dependency-free,
  and organized by responsibility without speculative package layers.

These are meaningful results. The findings below are defects in lifecycle and
boundary enforcement, not evidence that the overall design is wrong.

## High-priority findings

### 1. EOF can overtake a response that was already read

Location: `conn.go:212-223`, `conn.go:319-375`, and `conn.go:541-570`.

The read loop enqueues responses for the ordered goroutine. If the next read
immediately returns EOF, `finish` closes `done` before the ordered goroutine has
delivered the queued response. The waiting call then selects nondeterministically
between its reply channel and `done`; if `done` wins, it returns
`ErrConnectionClosed` even though the successful response was already received.

This occurred during review: the first `go test -race -count=1 ./...` run failed
`TestTheCommandTransportStartsAndReapsAnAgent` because `Client.Connect` received
`acp: the connection is closed`. Repeated runs passed, which is consistent with a
scheduling-sensitive ordering race rather than a deterministic test failure.

Required outcome:

- a message accepted by `Read` is processed before connection termination becomes
  observable to waiting calls;
- responses retain their ordering after earlier notifications;
- EOF still terminates the connection after the accepted queue is drained;
- a deterministic test makes a transport return one response followed
  immediately by EOF and proves the response wins every time.

### 2. Request cancellation can hold the caller for five extra seconds

Location: `conn.go:18-24` and `conn.go:359-412`.

When a call context finishes, `call` retires the pending request and invokes
`cancelRemote` synchronously. That function may spend up to five seconds blocked
writing `$/cancel_request`. The original caller therefore does not necessarily
receive `ctx.Err()` at its own cancellation or deadline, despite the comments
claiming that the caller has already returned.

The cancellation notification is courtesy to the peer and must have an
independent budget, but that budget must not be added to the completed caller's
latency. Send it from connection-owned asynchronous work whose exit is tracked by
`Wait`, while the original call returns immediately after retiring its pending
entry.

Add a transport that deliberately blocks cancellation writes and prove the
original call returns before the independent cancellation budget expires.

### 3. Turn cancellation has registration races on both peers

Location: `cancel.go:27-95` and `cancel.go:98-170`.

On the client, `cancelPermissions` clones the currently tracked permission IDs
and releases the lock. A new `session/request_permission` may register after the
snapshot, or cancellation may occur after the JSON-RPC request becomes inflight
but before `trackPermission` runs. That request survives `ClientSession.Cancel`
and a late user decision can still win after cancellation returned.

On the agent, `trackTurn` unconditionally overwrites the request ID for a
session. This creates two defects:

- an arbitrary peer can send a second concurrent prompt even though the public
  client handle promises one active prompt per session;
- a `session/cancel` notification may run before the prompt goroutine records its
  turn, observe no active turn, and be lost permanently.

Cancellation and registration need one connection-owned session state machine.
Starting cancellation must atomically prevent new permission registrations from
escaping it. Starting a prompt must atomically claim the session's single turn,
and a second prompt must receive the documented refusal rather than overwrite
the first turn.

Add deterministic tests for:

- permission registration racing the cancellation snapshot;
- prompt receipt immediately followed by `session/cancel`, before the handler
  starts;
- two prompt requests for the same session sent by a custom or TypeScript peer.

### 4. Capability validation covers only part of the advertised surface

Location: `client.go:133-192`, `agent.go:81-118`, and
`session.go:153-170`.

`AgentConfig.resolveCapabilities` checks only `loadSession`. A caller can still
advertise unimplemented method capabilities such as session listing, deletion,
logout, providers, or NES. `ClientConfig.resolveCapabilities` checks only
filesystem and terminal handlers, so it can similarly advertise unimplemented
client methods such as elicitation. The inbound gate then refuses a method the
peer was explicitly told would be available.

There are two related holes:

- `ClientConn.LoadSession` does not perform its promised outbound capability
  check before writing; the remote gate is currently what refuses it;
- construction shallow-copies config. Mutable values such as
  `*TerminalHandlers`, `AuthMethods`, metadata maps, and capability-owned slices
  remain aliased to caller memory and can invalidate the checked configuration or
  race with a connection after construction.

Make construction validate every method-bearing capability in the complete
capability table. Capabilities describing data accepted by an existing handler
may remain explicit refinements, but capabilities promising a separate method
must have the complete handler group. Copy every mutable configuration value the
library continues to read after construction.

Tests should mutate the original config after construction and prove the
client/agent behavior and advertisement do not change.

### 5. Initialization is neither complete nor closed as a lifecycle

Locations: `peer.go:3-29`, `client.go:272-303`, and `agent.go:121-145` plus
`agent.go:209-224`.

`InitializeResponse.authMethods` is decoded but discarded. `Client.Connect` does
not return the full initialize response, and `PeerInfo` has no authentication
methods. A client therefore cannot discover the allowed
`AuthenticateRequest.methodId`; current tests work only by hard-coding an ID
already known out of band.

`PeerInfo` also calls itself an immutable snapshot but is a shallow struct copy.
Nested maps and slices may still alias the capability authority held by the
connection. The snapshot omits both initialize `_meta` values as well.

There is also a pre-initialize outbound path. `Agent.Connect` intentionally
returns before initialize arrives, but the returned `AgentConn` immediately
exposes `Call` and `Notify`. A caller can therefore send an extension request
before the client has initialized and before capabilities are known.

Required outcome:

- preserve the complete negotiated initialization facts, including auth methods
  and metadata;
- return defensive copies or immutable projections for mutable nested state;
- reject `AgentConn.Call` and `AgentConn.Notify` until initialization completes,
  or expose an explicit initialization milestone that callers must await;
- validate that non-empty `AuthMethods` cannot be paired with a missing
  authentication implementation.

### 6. Protocol versions are not ordered by taking their minimum

Location: `agent.go:246-282` and `client.go:285-302`.

The agent currently selects `min(request.ProtocolVersion,
CurrentProtocolVersion)`. This package implements protocol version 1 only. If a
client requests version 0, the agent returns 0 and claims support for a grammar it
does not implement. The client then accepts it because it rejects only versions
greater than 1.

The pinned schema says an agent returns the requested version when supported, or
its latest supported version otherwise; the client disconnects if it cannot
support the answer. Protocol numbers identify incompatible grammars, not a
numeric feature level whose minimum is automatically safe.

For the current single-version package, the negotiated result must be exactly
`CurrentProtocolVersion`. Add tests for request and response versions 0, 1, and a
future higher version, with the expected response or disconnect behavior derived
from the pinned schema.

### 7. Generated method shape is recorded but not enforced

Location: `methods.gen.go:76-141`, `conn.go:212-266`, and
`conn.go:482-503`.

The generated method table records whether every standard method is a request, a
notification, or either, but dispatch never reads that field. It only checks
whether the envelope contains an ID.

Consequences include:

- a request method such as `terminal/kill` sent as a notification still executes
  its side effect and produces no response;
- a notification method such as `session/update` sent as a call executes and is
  then answered with an internal error;
- a response carrying neither `result` nor `error` is accepted as success for
  every result type because `decodeResponse` treats an absent result as the zero
  value.

Validate standard method shape before dispatching any handler. A malformed
notification cannot receive an error response, but it must not execute a request
handler. A successful JSON-RPC response must carry `result`; an empty object is a
valid result, while a missing member is not.

Add raw-message tests for both shape inversions, missing result, both result and
error, and a valid empty-object result.

### 8. Command transport shutdown is unbounded

Location: `transport_stdio.go:163-227`.

`commandConnection.Close` closes the pipes and calls `cmd.Wait` without a
deadline. An agent that ignores stdin EOF or keeps background work alive can
therefore block `ClientConn.Close`, failed connection setup, and any waiter
forever. This violates the connection rule that every owned goroutine and
resource has a defined exit path.

Use a bounded subprocess shutdown sequence. The official MCP Go SDK closes
stdin, waits for a configurable grace period, sends a termination signal, waits
again, and finally kills the process. ACP needs the same ownership result with
platform-specific signaling handled deliberately. Do not treat the eventual
process exit status as a connection failure after a local close.

Test a child that exits on EOF and one that ignores EOF and termination until it
is killed. Neither test should use an unbounded real-time sleep.

## Medium-priority finding

### 9. Error data bypasses the three-state representation

Location: `conn.go:456-493`.

Generated `Error.Data` is `Opt[json.RawMessage]`, but the JSON-RPC adapter does
not preserve its states:

- outbound `OptNull` makes `Get` return false, so explicit `data: null` is omitted;
- inbound raw `null` is wrapped with `OptValue` rather than `OptNull`.

The wire remains parseable, but the public state and a relayed error differ from
what the peer sent. Check `IsNull` explicitly when producing the wire error and
restore `OptNull` when decoding raw `null`. Add absent, null, scalar, object, and
array cases around the connection adapter rather than only the generated
`Error` codec.

## Recommended repair order

The defects are coupled enough that fixing them in this order minimizes rework:

1. Separate "read side ended" from "all accepted messages were delivered", and
   make the EOF/last-response test deterministic.
2. Make request cancellation asynchronous from the caller, then introduce one
   session/turn state owner for prompt and permission registration.
3. Enforce the complete capability table during construction and on every
   outbound gated operation; remove mutable config aliases.
4. Complete the initialization snapshot and pre-initialize state gate, then fix
   exact protocol-version selection.
5. Enforce method and response envelope shapes before handlers or result decoding.
6. Bound subprocess termination.
7. Correct explicit-null error data and add the adapter-level fixtures.

After each step, keep the product working end to end rather than accumulating a
second unfinished connection implementation.

## Verification performed

The following passed during review:

- `go test -count=1 ./...`;
- `go vet ./...`;
- `go run ./internal/cmd/schemagen -check`;
- `gofumpt`, `golangci-lint`, and shell formatting;
- `govulncheck` and the repository reachability check;
- `go mod tidy -diff`;
- the documentation check;
- compile-only test runs for Windows and macOS.

The first `go test -race -count=1 ./...` run exposed the EOF/response race in
finding 1. A later repeated race run passed, so the existing suite is generally
healthy but does not yet make that ordering deterministic.

## Decision

The implementation is a credible first version and the generator/wire work can
remain. It is not yet safe to merge as the SDK's initial public implementation.
Resolve findings 1 through 8 and rerun the full race and TypeScript process
interop suites before treating the connection, cancellation, and capability
contracts as ready. Finding 9 should be fixed in the same pass because it is
small and directly contradicts the otherwise sound three-state model.
