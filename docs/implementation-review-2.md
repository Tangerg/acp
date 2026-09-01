# Go implementation review, round 2

Review of the revised implementation following
[`implementation-review.md`](./implementation-review.md).

**Status: not ready to merge or release.** The implementation has made
substantial progress: five findings from the first implementation review are
fully resolved, and the intended fixes for the other four are visible. The
remaining blockers do not require an architectural restart, but cancellation,
initialization, and connection failure handling still have paths that violate
their public contracts.

## Review basis

This pass reviewed the uncommitted working tree on top of
`89e096b5f723c7ca972d308310807ff978596d32` on 2026-09-01. Because the
implementation is still uncommitted, that commit identifies the design baseline
rather than an immutable implementation snapshot.

The review checked the implementation against:

- the pinned schema in `schema/schema.json`, release `schema-v1.21.0`;
- `~/Desktop/acp-typescript-sdk` at
  `5dac09aaae3ebde1eaaf4a11840f7543f4806e20`;
- the official MCP Go SDK at `~/Desktop/go-sdk`, commit
  `21c18c6229e1c6d1d53d9a57475a2f65cc508cf3`;
- the repository rules in [AGENTS.md](../AGENTS.md), especially that the
  published schema owns the wire grammar and that retained configuration must
  not change after validation.

No existing code or documentation was changed during the review. This report is
the only new file.

## Progress since round 1

The following first-round findings are fully resolved:

- a response read immediately before EOF is drained before connection
  termination becomes observable;
- cancelling an outbound call returns the caller's context error without adding
  the remote cancellation budget to its latency;
- protocol version negotiation accepts exactly the version this package
  implements;
- standard request and notification shapes, and the result-or-error response
  envelope, are enforced before payload handling;
- error `data` preserves absent, null, and value states through the JSON-RPC
  adapter.

The revised capability table, outbound `LoadSession` gate, authentication facts
in `PeerInfo`, agent-side initialization gate, and bounded subprocess shutdown
are also sound improvements. Their remaining gaps are described below rather
than counted as regressions against the work that now functions.

## High-priority findings

### 1. A permission request cannot be cancelled during registration

Location: `cancel.go:131-147` and `conn.go:305-335`.

`ClientConn.registerInbound` runs before `conn.receive` creates the request's
`inflight` entry. When the session is already cancelling,
`registerInbound` calls `answerCancelled`, which calls `claimResponse`.
`claimResponse` necessarily returns false because the request is not in
`inflight` yet.

Registration then returns without telling `conn.receive` to stop. The read loop
adds the request to `inflight`, dispatches it, and the application's
`RequestPermission` handler is invoked. Its later answer can win normally. This
is precisely the behavior the cancellation state was intended to prevent: a
permission dialog appears for a turn the user has already cancelled.

The existing test proves only that `registerPermission` returns false while the
session is cancelling. It does not exercise the subsequent failed
`claimResponse` and dispatch over a real connection.

Required outcome:

- establish the inbound request state before registration can claim its
  response;
- let registration explicitly choose between dispatch and immediate response;
- ensure an immediately cancelled permission response is written before the
  corresponding `session/cancel` notification;
- add a deterministic wire-level test in which the permission request arrives
  after cancellation has begun and prove the user handler never runs.

### 2. A rejected prompt can permanently claim its session

Location: `cancel.go:179-237`, `cancel.go:264-289`, and
`agent.go:361-406`.

The read loop calls `AgentConn.registerInbound` before initialization checks,
capability checks, and full parameter decoding. A `session/prompt` therefore
claims its session before the implementation knows that the request is
serviceable.

The only release is the `defer releaseTurn` installed inside `prompt`, after
full parameter decoding succeeds. At least two ordinary rejection paths bypass
it:

- a prompt sent before initialize claims the session and is then rejected by
  `AgentConn.serve`;
- a prompt with a decodable `sessionId` but malformed or incomplete remaining
  parameters claims the session and then fails `decodeParams`.

In both cases the request receives an error, but the session remains marked
`running`. Every later valid prompt for that session is refused as concurrent.
The same lifecycle mismatch can leave malformed permission requests recorded in
the client's pending map.

Registration cleanup must belong to the inbound request lifecycle, not only to
the successful handler path. Add tests that send a rejected prompt followed by
a valid prompt for the same session, for both pre-initialize and invalid-params
cases.

### 3. Caller cancellation is still confused with the end of a turn

Location: `session.go:52-113` and `cancel.go:117-129`.

`ClientSession.Prompt` unconditionally clears `prompting` and calls
`endCancelling` when the method returns. Its own contract says that cancelling
the context only stops this caller from waiting and does not end the remote
turn. The local state therefore announces a fact the protocol has not yet
established.

This produces three related defects:

- after a prompt context is cancelled, a second prompt is accepted locally even
  though the first remote turn may still be running;
- if the caller follows the documented sequence and calls `Cancel` after the
  cancelled `Prompt` has returned, `cancelling` is set with no outstanding
  `Prompt` left to clear it;
- permission requests from the next turn can consequently be answered as if
  they belonged to the cancelled turn.

There is also a race in the other direction: a prompt return can clear
`cancelling` while a concurrent `Cancel` is still establishing it, allowing a
late permission request from the cancelled turn through.

The client needs a generation-aware logical turn state that survives the
caller's wait context and ends only when the peer's prompt response is observed.
That may require keeping a private late-response waiter after the public call
returns. A single session Boolean cannot distinguish an old turn from the next
one.

Tests should cover context cancellation followed by `Cancel`, a new prompt, and
permission requests on both sides of the turn boundary.

### 4. Initialization is still open on the client side

Location: `client.go:252-267`, `client.go:313-353`,
`client.go:392-432`, and `agent.go:306-352`.

The read loop starts before the client has received and validated initialize,
but `ClientConn.serve` has no initialization phase check. A peer can therefore
invoke baseline methods such as `session/update` and
`session/request_permission`, or an extension fallback, while
`Client.Connect` is still running. These calls reach application code before
the negotiated peer exists.

The agent has the complementary response-order race. `AgentConn.initialize`
sets `initialized = true` before the connection writes the initialize response.
An external `AgentConn.Call` or `Notify` can observe the new state and race its
message ahead of the response that was supposed to establish it for the client.

Use an explicit connection phase shared with dispatch. The client should open
inbound dispatch only after it has accepted the initialize response, and the
agent should open outbound operations only after its initialize response has
been successfully written. A state set inside the handler is one step too
early.

Add raw-peer tests for a baseline call and an extension call before client
initialization, plus an agent call racing the initialize response write.

### 5. Response write and close failures do not reach the connection lifecycle

Location: `conn.go:574-613` and `conn.go:703-752`.

The `Connection` contract states that a failed read or write ends the logical
connection. Outbound calls and notifications route their write errors through
`writeFailure`, but handler responses do not. `writeResponse` logs a transport
write failure and returns, leaving the connection open.

If the output side of a stream has failed while its input remains open, the
connection can continue accepting requests it can never answer. `Wait` may then
block indefinitely because no terminal state was recorded.

Local close errors are lost too. `conn.close` always returns nil, while
`endReading` only logs `transport.Close` errors. A command transport that cannot
close a pipe, terminate the process, or reap it can therefore make
`ClientConn.Close` and `Wait` both report success. That contradicts
`commandConnection.Close`, whose documented error is specifically the failure
to release or reap the owned process.

Required outcome:

- send response write failures through the same terminal path as other write
  failures;
- preserve the first transport close or subprocess ownership failure;
- return it consistently from public `Close` and `Wait` according to one stated
  policy;
- test a connection whose response write fails while reads remain blocked, and
  a transport whose `Close` fails.

### 6. Connection-dependent initialize fields are not negotiated

Location: `agent.go:100-103`, `agent.go:306-350`,
`client.go:331-350`, and `session.go:184-197`.

Two initialize fields depend on what this particular client offered, but the
agent copies both from static configuration:

- the schema says an `AuthMethodTerminal` must be advertised only when the
  client enabled `clientCapabilities.auth.terminal`;
- `AgentCapabilities.positionEncoding` is the encoding selected by the agent
  from `ClientCapabilities.positionEncodings`.

The current initialize response sends every configured authentication method
and the configured position encoding without consulting the request. The client
also accepts a position encoding it never offered. This can produce a schema-
invalid authentication exchange or make the peers interpret document offsets
under different encodings.

Authentication construction has the inverse error. `NewAgent` requires an
`Authenticate` handler whenever `AuthMethods` is non-empty, but the schema says
the client must not pass a terminal method to `authenticate`. A valid
terminal-only agent is rejected, while the method that needs the handler is the
`AuthMethodAgent` arm.

Generate the initialize response per connection:

- filter terminal authentication methods unless the client opted in;
- require `Authenticate` only for agent-handled authentication methods;
- select an offered position encoding or omit the unstable field;
- have the client reject an unoffered selection;
- prevent `ClientConn.Authenticate` from sending a terminal method ID.

Cover terminal-only, agent-only, mixed, opted-in, and opted-out authentication,
plus empty and non-empty position-encoding intersections.

## Medium-priority findings

### 7. The defensive copies still share mutable state

Location: `client.go:164-184`, `client.go:210-232`,
`agent.go:112-159`, and `peer.go:49-72`.

The new clone functions cover the outer container but not the complete retained
value:

- `resolveCapabilities` returns a shallow copy, so the
  `Client.capabilities.PositionEncodings` slice still aliases the caller's
  original configuration even though `ClientConfig.clone` separately clones
  the unused copy inside the retained config;
- `slices.Clone([]AuthMethod)` clones only the interface slice, not the pointed-
  to authentication values, terminal argument slices, environment maps, or
  metadata;
- `Implementation.Meta` and nested capability metadata remain shared;
- `maps.Clone(Meta)` copies only the outer map, while a `Meta` value may contain
  nested maps and slices.

As a result, mutation after construction can still change a later initialize
message or race its encoding. A value returned by `Peer()` can also mutate facts
held by the connection, despite the method's immutable-snapshot contract.

Use one deliberate deep-copy boundary for JSON-shaped data and union values,
apply it both to resolved capabilities and retained config, and test mutation of
nested values rather than only replacement of outer slices or pointers.

### 8. Empty identifiers are rejected outside the schema

Location: `session.go:296-313` and `workspace.go:62-85`.

The pinned definitions of `SessionId` and `TerminalId` require a JSON string and
do not declare `minLength`. The TypeScript codec consequently accepts an empty
string. The implementation changes that grammar by treating an empty session or
terminal identifier as an internal error.

The repository rule is explicit: the schema owns the wire grammar. Remove these
checks unless the constraint is first added upstream and then arrives through a
new pinned schema release. If the library wants a stronger application-level
identifier type, it must not make schema-valid peer messages fail decoding or
dispatch implicitly.

### 9. The terminal handle does not enforce its release contract

Location: `workspace.go:88-161`.

`TerminalHandle.Release` documents that the handle is unusable afterwards, but
the handle retains no released state. `Output`, `WaitForExit`, `Kill`, and another
`Release` all continue sending requests.

Either enforce the contract atomically and define what concurrent release means,
or weaken the documentation and leave validity entirely to the remote client.
The current combination gives callers a local guarantee the implementation does
not provide.

## Recommended repair order

The first three findings share one state-model problem, so fixing them together
will avoid another layer of races:

1. Define connection initialization phases and per-session turn generations.
2. Move inbound registration, immediate response, dispatch, and cleanup into one
   request lifecycle.
3. Keep prompt completion observable internally after the caller's context has
   stopped waiting.
4. Route every response write and transport close failure through the terminal
   connection state.
5. Make initialize response construction connection-specific for authentication
   and position encoding.
6. Establish one complete defensive-copy boundary, then remove local identifier
   constraints the schema does not contain.
7. Decide and test the post-release terminal handle contract.

After steps 1 through 5, the implementation is ready for another focused review.
The remaining medium-priority items should be resolved before the public API is
tagged, because each is cheaper to fix before downstream callers depend on its
current contract.

## Verification performed

The working tree passed all checks run during this review:

- `go test -count=1 ./...`;
- `go test -race -count=1 ./...`;
- `go test -race -count=20 ./...`;
- `go vet ./...`;
- `go mod tidy -diff`;
- `golangci-lint` for Linux, Darwin, and Windows;
- `gofumpt` and `shfmt` checks;
- `go run ./internal/cmd/schemagen -check`;
- `scripts/check-reachability.sh`;
- `govulncheck ./...`;
- Darwin and Windows cross-compilation checks;
- the complete documentation check in `.tools`.

The green gate is useful evidence for formatting, generated-code stability,
ordinary behavior, and races exercised by the existing suite. It does not make
the findings above hypothetical: the first two follow deterministically from the
ordering of registration and `inflight` creation, and the missing tests do not
currently drive those complete paths.
