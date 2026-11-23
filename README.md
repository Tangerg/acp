<a href="https://agentclientprotocol.com/">
  <img alt="Agent Client Protocol" src="https://zed.dev/img/acp/banner-dark.webp">
</a>

# acp

[![Go Reference](https://pkg.go.dev/badge/github.com/Tangerg/acp.svg)](https://pkg.go.dev/github.com/Tangerg/acp)
[![CI](https://github.com/Tangerg/acp/actions/workflows/ci.yml/badge.svg)](https://github.com/Tangerg/acp/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/Tangerg/acp?logo=go&logoColor=white)](./go.mod)
[![Apache 2.0](https://img.shields.io/badge/licence-Apache--2.0-blue)](./LICENSE)

`acp` is a Go implementation of the [Agent Client
Protocol](https://agentclientprotocol.com). It includes both peers:

- A **client**, such as a code editor, owns the workspace and represents the user
- An **agent** uses a model and calls the client to read files, run commands, or
  request permission

Both peers share one message grammar. An agent serves prompts while making calls
back to the client, so each connection sends and receives requests.

Install the module with:

```sh
go get github.com/Tangerg/acp
```

Use the Go version declared in [go.mod](./go.mod) or newer.

## Start a client

Build a client from the handlers that an agent may call. Setting an optional
handler advertises its capability:

```go
client, err := acp.NewClient(&acp.ClientConfig{
	Info:              &acp.Implementation{Name: "an editor", Version: "1.0.0"},
	SessionUpdate:     renderUpdate,
	RequestPermission: askPermission,
	ReadTextFile:      readTextFile,
})
if err != nil {
	return err
}
```

Each client handler receives the originating `*acp.ClientConn` after its context,
then the protocol payload. A shared client can connect to several agents whose
session identifiers overlap; the connection supplies that missing scope.

Connect to an agent subprocess. `CommandTransport` owns the process and its pipes,
so use `exec.Command` rather than attaching the process to the handshake context:

```go
transport := acp.NewCommandTransport(&acp.CommandConfig{
	Command: exec.Command("some-agent"),
})
conn, err := client.Connect(ctx, transport)
if err != nil {
	return err
}
defer conn.Close()
```

Create a session and run one turn:

```go
session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{
	Cwd:        "/work",
	McpServers: []acp.McpServer{},
})
if err != nil {
	return err
}

answer, err := session.Prompt(ctx, &acp.PromptParams{
	Prompt: []acp.ContentBlock{
		&acp.TextContent{Text: "add a test for the parser"},
	},
})
```

If `NewSession` returns `acp.ErrAuthRequired`, choose one of the authentication
methods from `conn.Negotiated().AuthMethods`, authenticate, and retry. An
identifier the agent did not offer, or one naming a terminal method, is refused
with `acp.ErrNotAdvertised` before anything is sent.

## Start an agent

Build an agent from the operations a client may call:

```go
agent, err := acp.NewAgent(&acp.AgentConfig{
	Info:       &acp.Implementation{Name: "an agent", Version: "1.0.0"},
	NewSession: newSession,
	Prompt:     prompt,
})
if err != nil {
	return err
}

return agent.Run(ctx, acp.NewStdioTransport())
```

Every handler is given the handle its method is scoped to. `Prompt` and the other
session-scoped handlers receive an `*acp.AgentSession`, which streams
`session/update`, requests permission, accesses files, and runs terminal commands.
`DeleteSession` receives the `*acp.AgentSession` named by its required
`sessionId`. `NewSession`, `Authenticate`, `Logout`, and `ListSessions` have no
session in their wire params and receive an `*acp.AgentConn`, which is how a
handler answering `authenticate`, say, can still elicit input from the user.

`session/cancel` cancels the running `Prompt` handler's context, so `Cancel` is
optional; set it only to observe the notification.

Keep diagnostics on stderr because stdout carries the protocol stream.

## Run the examples

The programs in [`examples/`](./examples) communicate through a real subprocess
pipe:

```sh
mkdir -p /tmp/workspace
go run ./examples/client \
  -prompt "remember the release is on Friday" \
  -cwd /tmp/workspace
```

The agent requests permission before writing `/tmp/workspace/NOTES.md`. Enter `1`
to approve it. Interrupt the client during the turn to send `session/cancel`
without terminating either process: the example agent ignores the terminal's
interrupt and stops when its stdin closes.

Focused examples for cancellation, authentication, terminals, session
configuration, the session lifecycle, elicitation, extension methods, a custom
transport, `Opt` and `Meta` are part of the [package
documentation](https://pkg.go.dev/github.com/Tangerg/acp#pkg-examples). Every
package example runs under `go test`, so none of them can go stale.

## Behavioural contracts

Five protocol rules affect every integration:

- **One prompt per session**: `session/cancel` identifies a session, not a turn. A
  second overlapping prompt returns `acp.ErrPromptInProgress`
- **Prompt context cancellation is not turn cancellation**: cancelling the
  context stops the local caller waiting. Call `ClientSession.Cancel` to end the
  turn and require a cancelled stop reason
- **A cancelled turn still reports**: the agent may send final tool-call updates
  before it answers, so a client keeps accepting `session/update` after
  cancelling, and an agent sends those updates with a context that outlives the
  turn, such as `context.WithoutCancel`
- **Notification handlers cannot synchronously call the same connection**:
  notifications retain arrival order, and later responses wait behind them.
  Start independent work instead of waiting inside the handler
- **Capabilities grant authority**: the connection rejects outbound and inbound
  calls that were not advertised, and refuses to send a value the peer never
  advertised it could take: prompt content, MCP transports, system prompts,
  `additionalDirectories`, boolean configuration options, notice,
  compaction, plan and subagent updates, and an authentication method the agent
  did not offer. A local refusal wraps `acp.ErrNotAdvertised`, so it cannot be
  mistaken for the peer's answer. Construction also rejects capabilities that
  lack matching handlers
- **A handler answers with a response or an error**: one returning neither is
  answered `-32603` on its behalf, never with a `null` result

Every path the protocol carries must be absolute. This package refuses to send a
relative one, and accepts either POSIX or Windows form because the path describes
the peer's filesystem rather than this process's.

Session and terminal handles are bound to their connection. A session handle
holds nothing but its identifier and connection, so two handles for one session
are interchangeable. Persist protocol identifiers when you need to reopen a
resource on another connection; do not retain a handle past connection shutdown.

`Limits` bounds the protocol state a peer can make a connection hold. A breach
reports `acp.ErrLimitExceeded`: a full delivery backlog or too many inbound
requests ends the connection, while too many pending outbound calls only refuses
the new call.

## Choose a transport

Byte-stream transports frame newline-delimited JSON-RPC 2.0. A line that is not a
valid message is answered with parse-error or invalid-request, or fails the one
call it answers, and reading continues; only a broken stream ends the connection. The in-memory transport
carries messages directly so connection tests do not test the codec twice.

| Transport | Use |
| --- | --- |
| `acp.NewCommandTransport` | Start an agent subprocess and own its pipes with bounded shutdown |
| `acp.NewStdioTransport` | Serve ACP from an agent's stdin and stdout |
| `acp.NewIOTransport` | Frame byte streams with one explicit release owner in `acp.IOConfig` |
| `acp.NewInMemoryTransports` | Connect two peers in one process |
| `acp.Transport` | Implement another message transport under the documented concurrency contract |

`IOConfig.Reader` and `Writer` provide I/O; `Close` alone releases their resources
and must unblock pending reads and writes. For a shared `net.Conn`, set both
streams to that connection and `Close` to its `Close` method.

A write honours its context only while it waits for the stream. Once a message
starts it is finished, because a byte stream cannot take back half a message, so
cancelling one caller never ends a connection other sessions share.

`StdioTransport` never closes the process's standard streams. A blocked read is
abandoned instead, so `Agent.Run` returns when its context ends even while the
client holds stdin open.

`CommandTransport` sets an unset `exec.Cmd.WaitDelay` to the effective
`TerminationGrace` before starting the process. An explicit `WaitDelay` is
preserved. This bounds draining output pipes inherited by descendants; a custom
`Stderr` writer must still finish its own `Write` calls. Expected process exit
status is ignored during shutdown, while unexpected pipe-copy errors remain
observable.

## Protocol coverage

The SDK serves all 25 methods in the pinned schema:

- **Connection and authentication**: `initialize`, `authenticate`, `logout`, and
  `$/cancel_request`
- **Sessions and turns**: `session/new`, `session/load`, `session/resume`,
  `session/list`, `session/delete`, `session/close`, `session/prompt`,
  `session/cancel`, `session/set_mode`, `session/set_config_option`,
  `session/update`, and `session/request_permission`
- **Workspace**: `fs/read_text_file`, `fs/write_text_file`, and all five
  `terminal/*` methods
- **Elicitation**: `elicitation/create` and `elicitation/complete`

Elicitation is one feature rather than two methods. A client sets
`ElicitationHandlers.Form`, `ElicitationHandlers.URL`, or both, and each field
advertises the mode it serves; a mode with no handler is refused with
invalid-params. An agent names the mode and never the scope:
`AgentSession.CreateElicitation` elicits within its session, and
`AgentConn.CreateElicitation` elicits within the request being served.

Form schemas are compiled before a handler can display them or an agent can send
them. Answers use the same private compiled constraints, including declared
email, URI, date and date-time formats. A pure Go ECMAScript engine owns pattern
syntax and matching, with explicit parsing, compilation and execution budgets.
Budget exhaustion and cancellation are reported as errors, distinct from an
ordinary non-matching answer.

For URL mode, `accept` is consent to start the out-of-band interaction; it does
not say the page has finished. Decline, cancellation, and failure release the ID
immediately. An accepted ID remains unique on that connection until the agent
sends `elicitation/complete`. Completing is optional, so at
`Limits.OutstandingElicitations` the oldest accepted interaction is forgotten
rather than refusing new ones.

## Project status

The module is pre-1.0, so its Go API may change. Its protocol target is fixed:

| Property | Value |
| --- | --- |
| Protocol version | 1 |
| Schema release | [Pinned stable release](./schema/provenance.json) |
| Module version | `v0.3.0` |
| Minimum Go version | Declared in [go.mod](./go.mod) |

The [published
schema](https://github.com/agentclientprotocol/agent-client-protocol) defines the
wire grammar. If this implementation disagrees with that schema, the
implementation is wrong. Proposed grammar changes belong upstream rather than in
a local dialect.

The repository vendors `schema/schema.json` from the published release. The
generator commits the closure of `schema/manifest.json` to `schema.gen.go`, so
schema updates produce reviewable source diffs and `go get` needs no generator.
The schema tag, asset hashes and reference SDK commit have one owner in
[`schema/provenance.json`](./schema/provenance.json); scripts, CI and the generator
read it directly.

### The stable grammar only

Upstream publishes two schemas per release: `schema.json` and
`schema.unstable.json`, the second being the first plus whatever is being tried.
Experimental methods and fields belong to that separate grammar.

This module generates from the stable asset alone, pinned by tag and SHA-256. That
is what lets its exported surface be a published grammar and `gorelease` mean
something. The cost is real and worth knowing before you choose: **experimental
standard methods are not reachable through this module**, not even by hand. The
extension API only carries `_`-prefixed names the protocol reserves for
implementations.

If you need them, use a library generated from the unstable channel — the other
Go implementations are. What you can do here is what a real editor does: the
recorded Zed handshake advertises its own experiments through `_meta`, and `_meta`
plus `_`-prefixed extension methods are fully supported.

### The v2 draft

Upstream is drafting protocol version 2 and publishing it as
`schema-v2.0.0-beta.N` pre-releases alongside the stable v1 line. **This module does not speak
it, and that is enforced rather than incidental**: `acp.CurrentProtocolVersion` is
1, an agent built here answers 1 to any request, and a client built here closes a
connection whose agent answers anything else.

You can still talk to a peer that supports v2, as long as it also still supports
v1. The specification requires an agent to answer with the version the client
asked for when it supports that version, so a v2-capable agent asked for 1 answers
1 and the connection proceeds in v1. A peer that has dropped v1 is out of reach,
and there is no flag, option, or extension method that changes this — the
extension API covers `_`-prefixed methods, and what v2 changes is the standard
ones.

A new protocol major changes the standard method and payload grammar. Supporting
it inside the same API would require version branches throughout the operations
and competing definitions of the same message types.

An ACP version number is only bumped for breaking changes — the schema says so,
and says non-breaking ones arrive as capabilities instead. That is why every v1.x
schema release is protocol version 1 and why there is nothing to negotiate within
a major. Successive stable v1 schemas add capabilities, which the connection
checks in both directions.

So the module major will track the protocol major: when v2 stabilises this module
becomes `acp/v2` and speaks version 2 only. Both can be imported side by side by
an editor that has to drive agents of either kind, which is what Go's major-version
rule is for. `design/design.md` records the argument and the cost.

Until then the pin stays on v1 and the pre-releases are watched rather than
implemented, because implementing a draft would encode a dialect that is still
changing. To follow it, watch the `schema-v2.0.0` pre-releases in the
[upstream repository](https://github.com/agentclientprotocol/agent-client-protocol/releases).

## Verification

Wire behaviour is checked against implementations this repository did not write, so a Go bug on both
sides of a test cannot hide:

- **Cross-SDK fixtures** replay what validators generated by the TypeScript SDK make of each input.
- **Subprocess interoperability** runs this client against an agent built on the reference SDK.
- **Editor interoperability** replays a recorded session against a real editor.
- **Concurrency** tests use `testing/synctest` and run under the race detector on Linux, macOS, and
  Windows.
- **Wire stability** fuzz targets require normalisation to reach a fixed point.
- **Examples** compile and run under `go test`.

How each corpus is produced and re-recorded is in
[CONTRIBUTING.md](./CONTRIBUTING.md#interoperability-evidence).

## Documentation

- [Package reference](https://pkg.go.dev/github.com/Tangerg/acp): exported API
  contracts and runnable examples
- [Design decisions](./design/design.md): ownership, state machines, wire fidelity,
  and rejected alternatives
- [Contributing](./CONTRIBUTING.md): toolchain, checks, interoperability evidence,
  and releases
- [Vendored schema](./schema/README.md): provenance, generation scope, and pin
  updates
- [Changelog](./CHANGELOG.md): caller-visible changes
- [Security](./SECURITY.md): private vulnerability reporting
- [Repository rules](./AGENTS.md): architectural constraints for every change

## Licence

The module is available under Apache-2.0. See [LICENSE](./LICENSE) and
[NOTICE](./NOTICE).

The banner comes from
[agentclientprotocol.com](https://agentclientprotocol.com/). This independent
implementation is not affiliated with the protocol's authors.
