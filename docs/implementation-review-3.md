# Go implementation review, round 3

This report reviews the implementation at `11767e4` after the changes made in response to [`implementation-review-2.md`](./implementation-review-2.md). It focuses on whether the current lifecycle and protocol state machines are ready to merge and extend.

**Status: continue implementation on the current architecture, but do not merge or release yet.** Five findings from round 2 are resolved. The remaining work is concentrated in three protocol-ordering blockers and two validation or value-copy defects. None requires an architectural restart.

## Review basis

This pass reviewed commit `11767e4a` on 2026-09-01. The work tree was clean before and after the review.

The review checked the implementation against:

- the pinned schema in `schema/schema.json`, release `schema-v1.21.0`
- `~/Desktop/acp-typescript-sdk`, including its matching stable schema
- the connection contract retained from the official MCP Go SDK model
- the repository rules in `AGENTS.md`
- the public contracts and findings recorded in the two earlier implementation reviews

The following checks passed:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `golangci-lint run ./...`
- `govulncheck ./...`
- `deadcode -test ./...`
- `gofumpt -l .`
- `shfmt -d scripts`
- `go mod tidy -diff`
- `go run ./internal/cmd/schemagen -check`
- `scripts/check-reachability.sh`
- the npm audit, spelling, and Markdown checks under `.tools`

These results establish that the current failures are not formatting, static-analysis, generated-code, or ordinary data-race failures. They are protocol-ordering cases that the existing tests do not exercise.

## Progress since round 2

The following round-2 findings are resolved:

- inbound permission requests are registered before cancellation can claim their response
- rejected and malformed prompts release their per-session turn claim through the request lifecycle
- terminal authentication methods and position encodings are negotiated per connection
- empty schema identifiers are no longer rejected by a local wire dialect
- terminal handles enforce release as an irreversible, concurrency-safe operation

The implementation also improves the remaining areas:

- caller cancellation now leaves a generation-aware waiter behind until the prompt response arrives
- the agent uses an outbound handshake barrier instead of a phase flag
- response writes and transport release failures enter the connection lifecycle on the ordinary error path
- generated values, capabilities, authentication methods, and nested JSON-shaped metadata are copied recursively

Four round-2 topics still have narrower defects. The first three remain merge blockers.

## High-priority findings

### 1. The client can reject a valid message sent after initialize

Locations: `link.go:119-124`, `link.go:166-177`, `client.go:284-334`, and `client.go:351-362`.

The agent now waits until its initialize response write completes before opening outbound operations. The client does not have the matching response-processing barrier.

The failure sequence is:

1. The client read loop receives the initialize response and appends it to the delivery queue.
2. The delivery loop passes the response to a buffered call channel.
3. The delivery loop or read loop starts handling the next notification or request.
4. The goroutine running `ClientConn.initialize` has not necessarily resumed, validated the response, or called `negotiated`.
5. `ClientConn.serve` observes `initialized == false` and rejects a message that the agent sent after its initialize response was on the wire.

Requests expose the race directly because `receive` dispatches them outside the ordered queue. Notifications can also lose because delivering a response to a buffered channel does not wait for the receiving goroutine to publish the negotiated state.

Required outcome:

- publish the validated client handshake as part of ordered response delivery, or add an acknowledgement that prevents later inbound dispatch until publication completes
- keep rejecting messages that actually precede initialize
- add a deterministic test in which an agent operation waits on its handshake barrier and sends immediately after the initialize response write
- cover both an extension request and an extension notification because they take different dispatch paths

### 2. An old Cancel can cancel the next turn

Locations: `cancel.go:57-85`, `cancel.go:127-146`, and `session.go:150-171`.

`ClientSession.Cancel` marks the session as cancelling, answers pending permission requests, and then writes `session/cancel`. `endTurn` clears `cancelling` as soon as the old prompt response is observed. Nothing records that the old `Cancel` call still owes its notification to the peer.

The failure sequence is:

1. Turn A is running and `Cancel` starts.
2. The old prompt responds before `Cancel` writes its notification. A permission response can make this timing likely by unblocking the agent.
3. `endTurn` releases turn A and clears `cancelling`.
4. Turn B starts and writes a new prompt.
5. The old `Cancel` resumes and writes `session/cancel`.
6. The notification names only the session, so the agent applies it to turn B.

The session API states that handles are safe for concurrent use, so requiring callers to wait for `Cancel` before observing or starting another turn would contradict the current contract.

Required outcome:

- bind cancellation progress to the turn generation it belongs to
- prevent `beginTurn` from admitting a new prompt until that generation's cancellation notification write completes
- handle two concurrent `Cancel` calls without letting the first completion reopen the session while the second still writes
- add a deterministic test that releases the old prompt before the cancellation notification and attempts the next prompt in that interval

### 3. Some write failures still leave the connection alive

Locations: `link.go:385-423` and `link.go:444-456`.

`writeFailure` sends ordinary transport errors through `endReading`, but it exempts `ErrConnectionClosed`, `context.Canceled`, and `context.DeadlineExceeded`.

The `ErrConnectionClosed` branch calls `life.failure` without first ending the read side. If a transport reports its closed output while its input remains blocked, `life.failure` waits for `delivered`, and nothing initiates the state transition that closes it.

The context branches are also unsafe for response writes. `writeResponse` creates its own five-second context. If that deadline expires, `writeFailure` treats it as the caller's cancellation and leaves the connection alive even though a response was not written. The request lifecycle then runs `answered`, which can open agent outbound operations after the initialize response failed to reach the client.

Required outcome:

- distinguish a caller context that expires before an outbound operation writes from a library-owned response-write deadline
- ensure every failed handler-response write ends the logical connection
- make `ErrConnectionClosed` initiate termination when no terminal transition has occurred
- call `answered` only after a response was successfully written
- add tests for a response write returning `context.DeadlineExceeded` and a write returning `ErrConnectionClosed` while reads remain blocked

## Medium-priority findings

### 4. Authenticate accepts an unadvertised method identifier

Locations: `session.go:241-267`, `agent.go:108-127`, and `agent.go:427-430`.

The schema requires `AuthenticateRequest.methodId` to name one of the authentication methods advertised in the initialize response. `ClientConn.Authenticate` rejects a matching terminal method, but it sends the request when no advertised method matches the identifier.

A raw client can also send an unknown identifier directly to an agent. `AgentConn.serve` dispatches it to the application handler without checking the per-connection advertised list.

Duplicate configured identifiers create a related ambiguity. The schema describes each authentication method identifier as unique, but `NewAgent` does not enforce uniqueness. A terminal and agent-handled method with the same identifier make the client reject the agent-handled flow because it encounters the terminal match.

Required outcome:

- reject client calls unless exactly one advertised `AuthMethodAgent` matches the identifier
- validate the same condition on agent ingress for peers that bypass this package's client
- reject duplicate authentication method identifiers during agent construction
- add unknown, terminal, duplicate, and valid agent-method tests

### 5. Deep copying can corrupt values stored in Meta

Locations: `meta.go:3-14` and `clone.go:52-118`.

`Meta` accepts `map[string]any`, while `copyValue` recursively creates a new struct and skips fields reflection cannot set. A JSON-marshallable value with unexported fields is therefore not preserved. For example, `time.Time` contains only unexported state, so copying it produces the zero time and changes the value sent on the wire.

The same boundary claims that a `PeerInfo` snapshot shares nothing mutable. That promise cannot be implemented for arbitrary Go values without either a constrained value model or a type-specific copy contract.

Required outcome:

- define and validate the Go value forms accepted inside `Meta`, then copy that closed set
- alternatively, canonicalize metadata through JSON and document that callers receive equivalent JSON values rather than their original Go dynamic types
- add a test for a JSON-marshallable struct with unexported state and a test for an unsupported mutable value

## Low-priority documentation finding

### 6. PeerInfo still documents minimum-version negotiation

Location: `peer.go:13-16`.

`PeerInfo.ProtocolVersion` says the negotiated version is the lower of the two versions. The implementation now correctly accepts exactly `CurrentProtocolVersion`, and the surrounding initialization documentation explains why. Update this exported comment so callers do not implement the obsolete minimum-version rule.

## Readiness assessment

The current architecture is viable. The split between `link`, `lifetime`, `calls`, `requests`, and `queue` gives each concurrency invariant a clearer owner than the previous combined connection type. The generation-aware turn model and the agent handshake channel are also the correct foundation.

Fix findings 1 through 3 before merging or adding more protocol surface. They affect message attribution and connection termination, so later features would inherit their races. Findings 4 and 5 should also be resolved before release because the schema owns authentication selection and the public snapshot contract currently overstates what `Meta` can preserve.

After those changes, rerun the existing quality gate and the new deterministic lifecycle tests. If they pass, this implementation is ready for the next capability layer.
