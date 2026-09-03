// Package acp implements both peers of the Agent Client Protocol.
//
// A client owns a workspace and represents its user. An agent serves prompts and
// calls the client to read files, run commands, or request permission. Both peers
// therefore send and receive JSON-RPC 2.0 requests on the same connection.
//
// # Building a client
//
// Build a client from the handlers an agent may call, then connect it to an agent
// transport:
//
//	client, err := acp.NewClient(&acp.ClientConfig{
//		SessionUpdate:     render, // the agent's running commentary for a turn
//		RequestPermission: ask,    // the user's decision, which is the client's alone
//	})
//
//	transport := acp.NewCommandTransport(&acp.CommandConfig{
//		Command: exec.Command("some-agent"),
//	})
//	conn, err := client.Connect(ctx, transport)
//	defer conn.Close()
//
//	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{
//		Cwd:        "/work",
//		McpServers: []acp.McpServer{},
//	})
//	answer, err := session.Prompt(ctx, &acp.PromptParams{
//		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "add a test for the parser"}},
//	})
//
// # Building an agent
//
// Build an agent from the operations a client may call, then run it over
// [NewStdioTransport]. Use [NewInMemoryTransports] when both peers run in one
// process.
//
// # Connections, sessions and turns
//
// A [Client] or [Agent] owns handlers and capabilities and may hold many
// connections. One connection may carry many sessions. Session and terminal
// handles are connection-bound; persist their protocol identifiers when a
// resource must be reopened on another connection.
//
// A turn is one [ClientSession.Prompt]. While it is outstanding the agent streams
// [SessionNotification] updates and calls back into the client. Two rules follow
// from the protocol rather than from this implementation, and a caller has to
// know both:
//
//   - One prompt at a time per session. session/cancel names a session and not a
//     turn, so a session with two turns could not say which one a cancellation
//     meant. A second overlapping prompt returns [ErrPromptInProgress].
//   - A notification handler must not synchronously call the same connection.
//     Notifications retain arrival order, so a handler that waits for a later
//     response waits for itself. Start independent work instead.
//
// Cancelling a Prompt's context and calling [ClientSession.Cancel] are different
// operations. The first stops the local caller waiting. The second ends the turn
// and requires the agent to answer with the cancelled stop reason.
//
// # Capabilities
//
// Capabilities are an authority boundary. Each side advertises what it serves
// during initialize. The package rejects unadvertised calls in both directions
// and rejects configurations that advertise methods without matching handlers.
//
// # What is served
//
// The package serves every method in the pinned schema. A method a later schema
// adds is refused with method-not-found until it is served, rather than being
// absent from the capability gate.
//
// # The wire types are generated
//
// Public wire types are generated from the vendored published schema and
// committed as schema.gen.go. Generated doc comments preserve the schema's prose.
//
// # Protocol version
//
// This package speaks protocol version 1 and refuses any other, because a
// protocol number names a grammar rather than a feature level. An agent built
// here answers 1; a client built here closes a connection whose agent answers
// otherwise. A peer that supports both still works, since the specification
// requires an agent to answer with the version its client asked for.
//
// Upstream is drafting version 2, and it is a different grammar rather than an
// increment: eleven of the twenty-five methods go, two of them returning under
// new names. This package will speak it as a separate major version of the
// module rather than as a negotiated mode; README.md has what to expect and
// design/design.md has the argument.
package acp

//go:generate go run ./internal/cmd/schemagen
