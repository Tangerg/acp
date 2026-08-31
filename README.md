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

Requires Go 1.27.

## Status

Early. The module is pre-1.0 and the API is expected to change; the protocol version
it targets is not.

| | |
| --- | --- |
| Protocol version | 1 |
| Module version | unreleased |
| Go floor | 1.27 |

## What it is

Messages are JSON-RPC 2.0 over a byte stream. The transport is ordinarily the agent
subprocess's stdin and stdout, but nothing here requires that: anything that can
carry newline-delimited JSON in both directions will do, which is what makes the
same code testable over an in-memory pipe.

The wire grammar is not this repository's to design. Where this implementation and
the [published schema](https://github.com/agentclientprotocol/agent-client-protocol)
disagree, the schema is right and this is a bug.

## Documentation

- [AGENTS.md](./AGENTS.md) — the rules this repository is built under
- [DESIGN.md](./DESIGN.md) — the Go shape of the protocol, and why
- [CONTRIBUTING.md](./CONTRIBUTING.md) — toolchain, the gate, and how to release
- [CHANGELOG.md](./CHANGELOG.md) — caller-visible changes
- [SECURITY.md](./SECURITY.md) — reporting a vulnerability privately

## Licence

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
