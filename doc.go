// Package acp implements the Agent Client Protocol.
//
// The protocol standardises the conversation between a client — a code editor or
// any other program that owns a workspace and a user — and an agent, a program
// that uses a model to read and change that workspace. Both halves live here,
// because they are one message grammar read from opposite ends: an agent
// answering a prompt is simultaneously a caller, asking the client to read files,
// run commands and put a permission dialog in front of its user.
//
// Messages are JSON-RPC 2.0. The transport is ordinarily the agent subprocess's
// stdin and stdout, but nothing here requires that; see [Transport].
//
// # Building a client
//
//	client, err := acp.NewClient(&acp.ClientConfig{
//		SessionUpdate: func(ctx context.Context, n *acp.SessionNotification) {
//			// The agent's running commentary for a turn.
//		},
//		RequestPermission: func(ctx context.Context, r *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
//			// Ask the user. There is no outcome this package may assume for you.
//			return &acp.RequestPermissionResponse{
//				Outcome: &acp.SelectedPermissionOutcome{OptionID: r.Options[0].OptionID},
//			}, nil
//		},
//	})
//
//	conn, err := client.Connect(ctx, acp.NewCommandTransport(&acp.CommandConfig{
//		Command: exec.CommandContext(ctx, "some-agent"),
//	}))
//	defer conn.Close()
//
//	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
//	answer, err := session.Prompt(ctx, &acp.PromptParams{
//		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "add a test for the parser"}},
//	})
//
// # Building an agent
//
// The same shape from the other end: an [AgentConfig] whose handlers are the
// operations a client calls, and [Agent.Run] over [NewStdioTransport].
//
// # Connections, sessions and turns
//
// A [Client] or [Agent] holds the handlers and the advertisement and may open
// many connections. A [ClientConn] is one link; a [ClientSession] is one
// conversation on it, and a connection carries many. Handles are
// connection-bound: a [SessionID] outlives the link it came from, a handle does
// not.
//
// A turn is one [ClientSession.Prompt]. While it is outstanding the agent streams
// [SessionNotification] updates and calls back into the client. Two rules follow
// from the protocol rather than from this implementation, and a caller has to
// know both:
//
//   - One prompt at a time per session. session/cancel names a session and not a
//     turn, so a session with two turns could not say which one a cancellation
//     meant. A second overlapping prompt returns [ErrPromptInProgress].
//   - A notification handler must not make a call on the same connection and wait
//     for it. Notifications are delivered in arrival order and a response is
//     delivered only after every notification that preceded it, so a handler that
//     waited for its own response would wait for itself. Spawn the work instead;
//     the session handle is valid beyond the handler call for exactly that.
//
// Cancelling a Prompt's context and calling [ClientSession.Cancel] are different
// operations. The first stops this caller waiting; the second ends the turn, and
// obliges the agent to answer the outstanding prompt with the cancelled stop
// reason. See [ClientSession.Cancel].
//
// # Capabilities
//
// Capabilities are an authority boundary rather than a feature list: they decide
// whether an agent may read a file or run a command. Each side advertises during
// initialize what it can serve, and this package enforces that in both
// directions — a call the peer never advertised is refused before it is sent, and
// one that arrives for a method this side never advertised is refused before it
// reaches a handler. A configuration that advertises what its handlers cannot
// serve fails at construction rather than on the first call.
//
// # What is served
//
// Every method the specification defines is classified. All are served except
// elicitation/create and elicitation/complete, which are refused with
// method-not-found rather than silently absent.
//
// # The wire types are generated
//
// The protocol's stable type definitions are read from schema/schema.json,
// vendored from a published upstream release, and the public closure is generated
// and committed as schema.gen.go. A schema change is therefore a reviewable source
// diff rather than a build-time download, and the generated doc comments preserve
// the specification's own prose.
package acp

//go:generate go run ./internal/cmd/schemagen
