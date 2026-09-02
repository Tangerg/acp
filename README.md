<a href="https://agentclientprotocol.com/">
  <img alt="Agent Client Protocol" src="https://zed.dev/img/acp/banner-dark.webp">
</a>

# acp

[![Go Reference](https://pkg.go.dev/badge/github.com/Tangerg/acp.svg)](https://pkg.go.dev/github.com/Tangerg/acp)
[![CI](https://github.com/Tangerg/acp/actions/workflows/ci.yml/badge.svg)](https://github.com/Tangerg/acp/actions/workflows/ci.yml)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Apache 2.0](https://img.shields.io/badge/licence-Apache--2.0-blue)](./LICENSE)

A Go implementation of the [Agent Client Protocol](https://agentclientprotocol.com).

The protocol standardises the conversation between a **client** — a code editor, or
any other program that holds a workspace and a user — and an **agent**, a program
that uses a model to read and modify that workspace. An editor that speaks it can
drive any agent; an agent that speaks it can be driven by any editor. Neither has to
know the other exists.

Both halves live in this module. They are one message grammar read from opposite
ends: an agent answering a prompt is simultaneously a caller, asking the client to
read files, run commands, and put a permission dialog in front of its user.

```sh
go get github.com/Tangerg/acp
```

Requires Go 1.25.

## A turn, from the client

```go
client, err := acp.NewClient(&acp.ClientConfig{
	Info: &acp.Implementation{Name: "an editor", Version: "1.0.0"},
	SessionUpdate: func(ctx context.Context, notification *acp.SessionNotification) {
		// The agent's running commentary: message chunks, tool calls, plans.
	},
	RequestPermission: func(ctx context.Context, request *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
		// Ask the user. There is no outcome this package may assume for you.
		return &acp.RequestPermissionResponse{
			Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[0].OptionID},
		}, nil
	},
	// Setting a handler is what advertises its capability, and an agent may not
	// call what this client did not advertise.
	ReadTextFile: readFile,
})
if err != nil {
	return err
}

conn, err := client.Connect(ctx, acp.NewCommandTransport(&acp.CommandConfig{
	Command: exec.CommandContext(ctx, "some-agent"),
}))
if err != nil {
	return err
}
defer conn.Close()

session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
if errors.Is(err, acp.ErrAuthRequired) {
	// Expected. Authenticate, then retry.
}

answer, err := session.Prompt(ctx, &acp.PromptParams{
	Prompt: []acp.ContentBlock{&acp.TextContent{Text: "add a test for the parser"}},
})
```

## A turn, from the agent

The same shape read from the other end. The handlers are the operations a client
calls, and the session handle is how an agent calls back while it answers:

```go
agent, err := acp.NewAgent(&acp.AgentConfig{
	Info: &acp.Implementation{Name: "an agent", Version: "1.0.0"},
	NewSession: func(ctx context.Context, request *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
		return &acp.NewSessionResponse{SessionID: mint(request.Cwd)}, nil
	},
	Prompt: func(ctx context.Context, session *acp.AgentSession, request *acp.PromptRequest) (*acp.PromptResponse, error) {
		session.Update(ctx, ...)            // stream what you are doing
		session.RequestPermission(ctx, ...) // before you touch the workspace
		session.WriteTextFile(ctx, ...)     // the client owns the files
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	},
	Cancel: func(ctx context.Context, session *acp.AgentSession, notification *acp.CancelNotification) {
		// Stop your own work. The turn's context is already cancelled.
	},
})
if err != nil {
	return err
}

// A client spawns the agent and owns both of its pipes, so stdout is the
// protocol stream and diagnostics belong on stderr.
return agent.Run(ctx, acp.NewStdioTransport())
```

## Run one

[examples/](./examples) has both of those as programs that spawn one another:

```sh
mkdir /tmp/workspace
go run ./examples/client -prompt "remember the release is on Friday" -cwd /tmp/workspace
```

The agent asks before it writes; answer `1`, and the note is in
`/tmp/workspace/NOTES.md`. Interrupt it mid-turn and the client sends
`session/cancel` rather than exiting, which is the distinction the protocol
insists on.

Smaller runnable examples — a turn, a cancellation, authentication, terminals,
extension methods, `Opt`, `Meta` — are in the [package
documentation](https://pkg.go.dev/github.com/Tangerg/acp#pkg-examples).

## What a caller has to know

Three rules come from the protocol rather than from this implementation, and no
API can hide them:

- **One prompt at a time per session.** `session/cancel` names a session and not a
  turn, so a session with two turns could not say which one a cancellation meant.
  A second overlapping prompt returns `acp.ErrPromptInProgress`.
- **Cancelling a prompt's context is not cancelling the turn.** The first stops
  this caller waiting; `ClientSession.Cancel` ends the turn and obliges the agent
  to answer the outstanding prompt with the cancelled stop reason. The connection
  guarantees that answer even when the agent's handler returns an abort error.
- **A notification handler must not make a call on the same connection and wait
  for it.** Notifications are delivered in arrival order, and a response is
  delivered only after every notification that preceded it, so a handler that
  waited for its own response would wait for itself. Spawn the work instead; a
  session handle stays valid beyond the handler call for exactly that.

Capabilities are the fourth thing worth knowing, and they are an authority
boundary rather than a feature list: they decide whether an agent may read a file
or run a command. Both directions are enforced here — a call the peer never
advertised is refused before it is sent, and one that arrives for a method this
side never advertised is refused before it reaches a handler. A configuration that
advertises what its handlers cannot serve fails at construction.

## Transports

Messages are JSON-RPC 2.0 over a byte stream. Ordinarily that stream is an agent
subprocess's stdin and stdout, but nothing here requires it:

| | |
| --- | --- |
| `acp.NewCommandTransport` | spawn an agent and own its pipes, with a bounded shutdown |
| `acp.NewStdioTransport` | an agent's own stdin and stdout |
| `acp.NewInMemoryTransports` | two peers in one process, which is what this package's tests run over |
| `acp.NewIOTransport` | any closeable pair of streams |
| `acp.Transport` | anything else; the concurrency and shutdown contract it is promising is on the interface |

## What is served, and what is not

Every method the schema defines is classified, and the two that are not served yet
are refused rather than silently absent: a peer calling one is answered
`method not found`, and an agent that tried to advertise one is refused at
construction.

Served: `initialize`, `authenticate`, `logout`, `session/new`, `session/load`,
`session/resume`, `session/list`, `session/delete`, `session/close`,
`session/prompt`, `session/cancel`, `session/set_mode`,
`session/set_config_option`, `session/update`, `session/request_permission`,
`fs/read_text_file`, `fs/write_text_file`, the five `terminal/*` methods, and
`$/cancel_request`.

Not served: `elicitation/create` and `elicitation/complete`. They are one feature
rather than two methods — a request carries a mode and a scope as two flattened
unions, URL mode answers asynchronously under an identifier of its own, and form
mode hands the client a JSON Schema to render — so they are a layer of their own
rather than an addition to this one.

## Status

Early. The module is pre-1.0 and the API is expected to change; the protocol
version it targets is not.

| | |
| --- | --- |
| Protocol version | 1 |
| Schema release | `schema-v1.21.0` |
| Module version | unreleased |
| Go floor | 1.25 |

The wire grammar is not this repository's to design. Where this implementation and
the [published schema](https://github.com/agentclientprotocol/agent-client-protocol)
disagree, the schema is right and this is a bug — and the fix belongs upstream
rather than in a local dialect.

The protocol's type definitions are read from `schema/schema.json`, vendored from
a published release, and the public closure is generated and committed as
`schema.gen.go`: 142 of the schema's 170 definitions. A schema change is therefore
a reviewable source diff rather than a build-time download, and `go get` needs no
generator.

## How this is known to work

- **A turn works end to end, in both directions** — over an in-memory pipe, over
  newline-delimited JSON, and against an agent built on the reference SDK running
  as a subprocess: a session, a prompt whose agent streams updates and asks
  permission in the middle, the filesystem and terminal operations, and both
  cancellations.
- **Four recorded interoperability transcripts.** Two Go endpoints talking to each
  other share any wire bug they have, so these are the other implementation's
  bytes: a turn, a cancelled turn whose final updates still arrive, authentication,
  and every capability-gated workspace method. `scripts/interop.sh` records them
  against the pinned reference SDK; `go test` replays them with no network and no
  Node.
- **A real editor, driving.** Zed 1.17.2 spawned an agent built on this module and
  a person drove it: a prompt, the permission dialog, a command through the
  editor's terminal, and the stop button. Those bytes are replayed by `go test`:
  every property the editor sent survives this package's types unchanged, and the
  turn the user stopped ends with the cancelled stop reason rather than a failed
  call.
- **125 cross-SDK fixtures**, whose expected outcomes come from the TypeScript
  SDK's own deserialisation machinery, regenerated from the pinned schema.
- **Two fuzz targets** asserting that normalisation is a fixed point, which is the
  property schema-directed recovery can quietly break.
- **Cancellation and shutdown tested with `testing/synctest`** rather than with
  sleeps, under `-race`, on Linux, macOS and Windows.
- **Every example in the package documentation runs under `go test`**, so one that
  goes stale fails the build rather than misleading a reader.

## Documentation

The reference is [pkg.go.dev](https://pkg.go.dev/github.com/Tangerg/acp), and the
reason for a decision is in the doc comment on the thing it decided. A separate
design document was kept while the design was being argued about; it drifted from
the code the moment the code started answering the same questions better.

- [AGENTS.md](./AGENTS.md) — the rules this repository is built under
- [CONTRIBUTING.md](./CONTRIBUTING.md) — toolchain, the gate, and how to release
- [schema/README.md](./schema/README.md) — the vendored schema, and what generation covers
- [CHANGELOG.md](./CHANGELOG.md) — caller-visible changes
- [SECURITY.md](./SECURITY.md) — reporting a vulnerability privately

## Licence

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

The banner is the protocol's own, served from
[agentclientprotocol.com](https://agentclientprotocol.com/). This module is an
independent implementation, not affiliated with the protocol's authors.
