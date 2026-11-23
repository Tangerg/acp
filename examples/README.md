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
`/tmp/workspace/NOTES.md`. The client holds an `os.Root`, so symlink resolution
and file I/O remain inside the open workspace directory. Only an actual missing
file is reported as missing; permission and I/O failures remain errors.

Text prompts and resource links are supported. A resource link contributes its
name and URI to the note; it does not trigger a file read or network fetch.

Appending requires both read and write capabilities. The agent stops on an
unreadable existing file and serializes the entire append for conversations using
the same file. Other processes and editors remain independent writers: standard
ACP file methods provide neither atomic append nor compare-and-swap.

Replace the example agent with any other ACP agent. Neither peer depends on the
other peer's implementation:

```sh
go run ./examples/client -prompt "hello" -- some-other-agent --flag
```

## Cancel a turn

Interrupt the client while a turn is running. It sends `session/cancel` rather
than exiting. The terminal delivers the interrupt to the agent too, because both
share its process group, so the agent ignores it and stops when its stdin closes.

Cancelling the prompt context only stops the client waiting. Cancelling the turn
requires the agent to stop and answer. The client resolves pending permission
requests as `cancelled`, and the agent reports a `cancelled` stop reason. Cancelling
while permission is pending prevents that operation from writing; it cannot undo
a write that already completed.

Only permission questions read stdin; the prompt itself comes from `-prompt`.
One process-owned input loop displays and reads each question in order. A cancelled
handler returns immediately even if stdin is blocked. Its late line is discarded
before a queued question is displayed, so an old approval cannot answer a new
request. An uninterruptible stdin read belongs to the program's lifetime, never
to a separate goroutine for each request.

## Find focused examples

The package documentation contains focused examples for a turn, cancellation,
authentication, terminals, extension methods, `Opt`, and `Meta`. Each example
runs both peers over an in-memory transport and executes under `go test`. See the
[package examples](https://pkg.go.dev/github.com/Tangerg/acp#pkg-examples) or
[`example_test.go`](../example_test.go).
