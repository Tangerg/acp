# acp

A Go implementation of the [Agent Client Protocol](https://agentclientprotocol.com).

The protocol standardises the conversation between a **client** — a code editor, or
any other program that holds a workspace and a user — and an **agent**, a program
that uses a model to read and modify that workspace. An editor that speaks it can
drive any agent; an agent that speaks it can be driven by any editor. Neither has to
know the other exists.

Both halves live in this module. A client drives an agent, and an agent serves a
client, because the two are the same message grammar read from opposite ends.

```sh
go get github.com/Tangerg/acp
```

Requires Go 1.25.

## Status

Early. The module is pre-1.0 and the API is expected to change; the protocol version
it targets is not.

| | |
| --- | --- |
| Protocol version | 1 |
| Schema release | `schema-v1.21.0` |
| Module version | unreleased |
| Go floor | 1.25 |

A turn works end to end, in both directions, over an in-memory pipe, over
newline-delimited JSON, and against an agent built on the reference SDK running as
a subprocess: initialize, a session, a prompt whose agent streams updates and asks
permission in the middle, the filesystem and terminal operations, and both
cancellations. That last one is the evidence that matters — two Go endpoints
talking to each other share any wire bug they have — and
[docs/roadmap.md](./docs/roadmap.md#4-turn) says how it is recorded and replayed.

## A turn

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

response, err := session.Prompt(ctx, &acp.PromptParams{
	Prompt: []acp.ContentBlock{&acp.TextContent{Text: "add a test for the parser"}},
})
```

The agent's side is the same shape read from the other end: an `acp.AgentConfig`
whose handlers are the operations a client calls, and `agent.Run(ctx,
acp.NewStdioTransport())`.

## What it is

Messages are JSON-RPC 2.0 over a byte stream. The transport is ordinarily the agent
subprocess's stdin and stdout, but nothing here requires that: anything that can
carry newline-delimited JSON in both directions will do, which is what makes the
same code testable over an in-memory pipe.

The wire grammar is not this repository's to design. Where this implementation and
the [published schema](https://github.com/agentclientprotocol/agent-client-protocol)
disagree, the schema is right and this is a bug.

## Documentation

Design notes, written before the code and meant to be argued with:

- [docs/protocol.md](./docs/protocol.md) — what ACP is: roles, the 43 methods, the shape of a turn
- [docs/design.md](./docs/design.md) — what it looks like in Go, and why that and not something else
- [docs/repository.md](./docs/repository.md) — how this repository is built and what each gate proves
- [docs/roadmap.md](./docs/roadmap.md) — the order of work, and what is still open

Sources for every claim in them — the specification, the TypeScript SDK, and the
official MCP Go SDK — are listed in [docs/README.md](./docs/README.md#sources).

And the usual:

- [AGENTS.md](./AGENTS.md) — the rules this repository is built under
- [CONTRIBUTING.md](./CONTRIBUTING.md) — toolchain, the gate, and how to release
- [CHANGELOG.md](./CHANGELOG.md) — caller-visible changes
- [SECURITY.md](./SECURITY.md) — reporting a vulnerability privately

## Licence

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
