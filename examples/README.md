# Run the client and agent examples

These two programs speak the protocol over a subprocess pipe. The client starts
the agent and owns its stdin and stdout.

- **[`agent`](./agent)**: streams a reply, requests permission, and appends the
  prompt to `NOTES.md`
- **[`client`](./client)**: starts the agent, renders the turn, asks the user for
  permission, and restricts file access to one directory

```sh
mkdir /tmp/workspace
go run ./examples/client -prompt "remember the release is on Friday" -cwd /tmp/workspace
```

The agent asks before it writes. Enter `1` to approve the request and write
`/tmp/workspace/NOTES.md`.

Replace the example agent with any other ACP agent. Neither peer depends on the
other peer's implementation:

```sh
go run ./examples/client -prompt "hello" -- some-other-agent --flag
```

## Cancel a turn

Interrupt the client while a turn is running. It sends `session/cancel` rather
than exiting.

Cancelling the prompt context only stops the client waiting. Cancelling the turn
requires the agent to stop and answer. The client resolves pending permission
requests as `cancelled`, the agent reports a `cancelled` stop reason, and no file
is written.

## Find focused examples

The package documentation contains focused examples for a turn, cancellation,
authentication, terminals, extension methods, `Opt`, and `Meta`. Each example
runs both peers over an in-memory transport and executes under `go test`. See the
[package examples](https://pkg.go.dev/github.com/Tangerg/acp#pkg-examples) or
[`example_test.go`](../example_test.go).
