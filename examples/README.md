# Examples

Two programs that speak the protocol to each other over a pipe between two
processes, which is how it is spoken in production: the client spawns the agent
and owns its stdin and stdout.

- [`agent`](./agent) — an agent binary. Streams a reply, asks permission before
  touching the workspace, and appends what it was told to `NOTES.md`.
- [`client`](./client) — an editor's side. Spawns the agent, renders the turn,
  asks the user about every permission request, and serves file reads and writes
  inside one directory and nowhere else.

```sh
mkdir /tmp/workspace
go run ./examples/client -prompt "remember the release is on Friday" -cwd /tmp/workspace
```

The agent asks before it writes. Answer `1`, and `/tmp/workspace/NOTES.md` has the
note in it.

Any other agent works in its place, which is the point of the protocol — neither
side knows which implementation it is talking to:

```sh
go run ./examples/client -prompt "hello" -- some-other-agent --flag
```

## Cancelling

Interrupt the client while a turn is running — while it is waiting for an answer
to a permission request, for instance — and it sends `session/cancel` rather than
exiting.

That is the difference the protocol insists on: cancelling the prompt's context
would only stop the client waiting, while cancelling the turn obliges the agent to
stop and answer. The pending permission request is answered `cancelled`, the agent
reports the `cancelled` stop reason, and nothing is written.

## Where the smaller examples are

The runnable examples in the package documentation cover one thing each — a turn,
a cancellation, authentication, terminals, extension methods, `Opt`, `Meta` — with
both halves in one process over an in-memory transport. See
[pkg.go.dev/github.com/Tangerg/acp](https://pkg.go.dev/github.com/Tangerg/acp) or
`example_test.go` in the repository root.
