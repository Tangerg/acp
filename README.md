<a href="https://agentclientprotocol.com/">
  <img alt="Agent Client Protocol" src="https://zed.dev/img/acp/banner-dark.webp">
</a>

# acp

[![Go Reference](https://pkg.go.dev/badge/github.com/Tangerg/acp.svg)](https://pkg.go.dev/github.com/Tangerg/acp)
[![CI](https://github.com/Tangerg/acp/actions/workflows/ci.yml/badge.svg)](https://github.com/Tangerg/acp/actions/workflows/ci.yml)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
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

The module requires Go 1.25 or newer.

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
methods from `conn.Peer().AuthMethods`, authenticate, and retry.

## Start an agent

Build an agent from the operations a client may call:

```go
agent, err := acp.NewAgent(&acp.AgentConfig{
	Info:       &acp.Implementation{Name: "an agent", Version: "1.0.0"},
	NewSession: newSession,
	Prompt:     prompt,
	Cancel:     cancelTurn,
})
if err != nil {
	return err
}

return agent.Run(ctx, acp.NewStdioTransport())
```

The `Prompt` handler receives an `*acp.AgentSession`. Use that handle to stream
`session/update`, request permission, access files, and run terminal commands.
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
without terminating the process.

Focused examples for cancellation, authentication, terminals, extensions, `Opt`,
and `Meta` are part of the [package
documentation](https://pkg.go.dev/github.com/Tangerg/acp#pkg-examples). Every
package example runs under `go test`.

## Behavioural contracts

Four protocol rules affect every integration:

- **One prompt per session**: `session/cancel` identifies a session, not a turn. A
  second overlapping prompt returns `acp.ErrPromptInProgress`
- **Prompt context cancellation is not turn cancellation**: cancelling the
  context stops the local caller waiting. Call `ClientSession.Cancel` to end the
  turn and require a cancelled stop reason
- **Notification handlers cannot synchronously call the same connection**:
  notifications retain arrival order, and later responses wait behind them.
  Start independent work instead of waiting inside the handler
- **Capabilities grant authority**: the connection rejects outbound and inbound
  calls that were not advertised, and refuses to send prompt content, MCP
  transports, or `additionalDirectories` the agent never advertised.
  Construction also rejects capabilities that lack matching handlers

Every path the protocol carries must be absolute. This package refuses to send a
relative one, and accepts either POSIX or Windows form because the path describes
the peer's filesystem rather than this process's.

Session and terminal handles are bound to their connection. Persist protocol
identifiers when you need to reopen a resource on another connection; do not
retain a handle past connection shutdown.

## Choose a transport

Byte-stream transports frame newline-delimited JSON-RPC 2.0. The in-memory
transport carries messages directly so connection tests do not test the codec
twice.

| Transport | Use |
| --- | --- |
| `acp.NewCommandTransport` | Start an agent subprocess and own its pipes with bounded shutdown |
| `acp.NewStdioTransport` | Serve ACP from an agent's stdin and stdout |
| `acp.NewIOTransport` | Use any closeable reader and writer |
| `acp.NewInMemoryTransports` | Connect two peers in one process |
| `acp.Transport` | Implement another message transport under the documented concurrency contract |

## Protocol coverage

The SDK serves 23 of the 25 methods in the pinned schema:

- **Connection and authentication**: `initialize`, `authenticate`, `logout`, and
  `$/cancel_request`
- **Sessions and turns**: `session/new`, `session/load`, `session/resume`,
  `session/list`, `session/delete`, `session/close`, `session/prompt`,
  `session/cancel`, `session/set_mode`, `session/set_config_option`,
  `session/update`, and `session/request_permission`
- **Workspace**: `fs/read_text_file`, `fs/write_text_file`, and all five
  `terminal/*` methods

`elicitation/create` and `elicitation/complete` are not implemented. The SDK
answers either call with method-not-found and rejects an advertisement that
claims elicitation support. Elicitation needs its own state machine because URL
mode completes asynchronously and form mode carries a JSON Schema for the client
to render.

## Project status

The module is pre-1.0, so its Go API may change. Its protocol target is fixed:

| Property | Value |
| --- | --- |
| Protocol version | 1 |
| Schema release | `schema-v1.21.0` |
| Module version | `v0.1.0` |
| Minimum Go version | 1.25 |

The [published
schema](https://github.com/agentclientprotocol/agent-client-protocol) defines the
wire grammar. If this implementation disagrees with that schema, the
implementation is wrong. Proposed grammar changes belong upstream rather than in
a local dialect.

The repository vendors `schema/schema.json` from the published release. The
generator commits 142 of its 170 definitions to `schema.gen.go`, so schema updates
produce reviewable source diffs and `go get` needs no generator.

## Verification

The repository checks the implementation through independent and adversarial
evidence:

- **Cross-SDK fixtures**: 125 cases use validators generated by the TypeScript SDK
  from the pinned schema
- **Subprocess interoperability**: four recorded transcripts run this client
  against an agent built with the reference TypeScript SDK
- **Editor interoperability**: a recorded Zed 1.17.2 session covers a prompt, a
  command run through the editor's terminal, and the stop button
- **Concurrency**: cancellation and shutdown tests use `testing/synctest` and run
  under the race detector on Linux, macOS, and Windows
- **Wire stability**: two fuzz targets require normalisation to reach a fixed point
- **Examples**: every package example compiles and runs under `go test`

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
