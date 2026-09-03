package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Tangerg/acp"
)

// Every example here runs both halves in one process over
// [acp.NewInMemoryTransports], because an example that needed a second process
// could not run — and one that does not run is one nobody notices going stale.
//
// Only the transport is different from a real deployment. A client spawns the
// agent with [acp.NewCommandTransport]; an agent's own main is
// [acp.NewStdioTransport] and [acp.Agent.Run]. Everything else on the page is
// what a real implementation writes. examples/ has that pair as two programs.

// A whole turn: a client drives an agent, and the agent calls back into the
// client's workspace while it answers.
func Example() {
	ctx := context.Background()

	agent, err := acp.NewAgent(&acp.AgentConfig{
		Info: &acp.Implementation{Name: "example-agent", Version: "0.1.0"},

		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},

		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			request *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			asked, ok := request.Prompt[0].(*acp.TextContent)
			if !ok {
				return nil, errors.New("this agent only understands text")
			}

			// Running commentary, for a client to render as it arrives.
			if err := session.Update(ctx, &acp.SessionUpdateParams{
				Update: &acp.AgentMessageChunk{ContentChunk: acp.ContentChunk{
					Content: &acp.TextContent{Text: "working on: " + asked.Text},
				}},
			}); err != nil {
				return nil, err
			}

			before, err := session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/work/README.md"})
			if err != nil {
				return nil, err
			}

			// Nothing that changes the workspace happens without asking. The client
			// owns the user, so the outcome is the client's to decide and there is
			// none this package may assume.
			decision, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
				ToolCall: acp.ToolCallUpdate{
					ToolCallID: "call-1",
					Title:      acp.OptValue("write /work/README.md"),
					Kind:       acp.OptValue(acp.ToolKindEdit),
				},
				Options: []acp.PermissionOption{
					{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				},
			})
			if err != nil {
				return nil, err
			}
			selected, chosen := decision.Outcome.(*acp.SelectedPermissionOutcome)
			if !chosen || selected.OptionID != "allow" {
				return &acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
			}

			if _, err := session.WriteTextFile(ctx, &acp.WriteTextFileParams{
				Path:    "/work/README.md",
				Content: before.Content + "Tidied.\n",
			}); err != nil {
				return nil, err
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},

		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		panic(err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		Info: &acp.Implementation{Name: "example-editor", Version: "0.1.0"},

		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			chunk, ok := notification.Update.(*acp.AgentMessageChunk)
			if !ok {
				return
			}
			if text, ok := chunk.Content.(*acp.TextContent); ok {
				fmt.Println("agent:", text.Text)
			}
		},

		RequestPermission: func(
			_ context.Context,
			request *acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			title, _ := request.ToolCall.Title.Get()
			fmt.Println("permission:", title)
			// A real client puts this in front of the user and waits.
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[0].OptionID},
			}, nil
		},

		// Setting these two is what advertises fs.readTextFile and fs.writeTextFile.
		// An agent may not call what a client did not advertise.
		ReadTextFile: func(_ context.Context, request *acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
			fmt.Println("read:", request.Path)
			return &acp.ReadTextFileResponse{Content: "# acp\n"}, nil
		},
		WriteTextFile: func(_ context.Context, request *acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
			fmt.Println("write:", request.Path)
			return &acp.WriteTextFileResponse{}, nil
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		panic(err)
	}

	answer, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "tidy up the README"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("stop reason:", answer.StopReason)

	// Output:
	// agent: working on: tidy up the README
	// read: /work/README.md
	// permission: write /work/README.md
	// write: /work/README.md
	// stop reason: end_turn
}

// Cancelling a turn is not cancelling the call that started it. The agent still
// owes an answer, and the protocol says what it is.
func ExampleClientSession_Cancel() {
	ctx := context.Background()
	started := make(chan struct{})

	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},

		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			close(started)
			<-ctx.Done()

			// The turn is over, but the client is still waiting for its answer and
			// still accepting updates until it arrives. This context is cancelled, so
			// a last word needs one that is not.
			if err := session.Update(context.WithoutCancel(ctx), &acp.SessionUpdateParams{
				Update: &acp.AgentMessageChunk{ContentChunk: acp.ContentChunk{
					Content: &acp.TextContent{Text: "stopping"},
				}},
			}); err != nil {
				return nil, err
			}
			// Nothing here reports the cancelled stop reason: the connection owes it,
			// and an abort error from a model or tool library is not a failed call.
			return nil, ctx.Err()
		},

		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {
			// Where an agent stops its own work. The turn's context is already
			// cancelled by the time this runs.
		},
	})
	if err != nil {
		panic(err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			if chunk, ok := notification.Update.(*acp.AgentMessageChunk); ok {
				if text, ok := chunk.Content.(*acp.TextContent); ok {
					fmt.Println("agent:", text.Text)
				}
			}
		},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return nil, errors.New("this example asks for nothing")
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		panic(err)
	}

	answered := make(chan acp.StopReason, 1)
	go func() {
		answer, failed := session.Prompt(ctx, &acp.PromptParams{
			Prompt: []acp.ContentBlock{&acp.TextContent{Text: "rewrite everything"}},
		})
		if failed != nil {
			panic(failed)
		}
		answered <- answer.StopReason
	}()

	// There is nothing to cancel until the turn exists.
	<-started
	if err = session.Cancel(ctx, nil); err != nil {
		panic(err)
	}
	fmt.Println("stop reason:", <-answered)

	// Output:
	// agent: stopping
	// stop reason: cancelled
}

// An agent that needs credentials answers session/new with -32000. That is a step
// in the lifecycle rather than a failure, and [acp.ErrAuthRequired] is how a
// client tells it apart from one.
func ExampleClientConn_Authenticate() {
	ctx := context.Background()
	var authenticated atomic.Bool

	agent, err := acp.NewAgent(&acp.AgentConfig{
		// What this agent will accept. The client reads it back from the handshake,
		// and calling authenticate with anything else is refused before it is sent.
		AuthMethods: []acp.AuthMethod{
			&acp.AuthMethodAgent{ID: "api-key", Name: "API key"},
		},
		Authenticate: func(context.Context, *acp.AgentConn, *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			authenticated.Store(true)
			return &acp.AuthenticateResponse{}, nil
		},

		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			if !authenticated.Load() {
				return nil, &acp.Error{
					Code:    acp.ErrorCodeAuthenticationRequired,
					Message: "set an API key first",
				}
			}
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		panic(err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return nil, errors.New("this example asks for nothing")
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	if _, _, err = conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"}); !errors.Is(err, acp.ErrAuthRequired) {
		panic("this agent was supposed to require authentication first")
	}

	for _, method := range conn.Peer().AuthMethods {
		if offered, ok := method.(*acp.AuthMethodAgent); ok {
			fmt.Printf("offered: %s (%s)\n", offered.ID, offered.Name)
		}
	}
	if _, err = conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "api-key"}); err != nil {
		panic(err)
	}

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		panic(err)
	}
	fmt.Println("session:", session.ID())

	// Output:
	// offered: api-key (API key)
	// session: session-1
}

// A terminal is the client's process, run on the agent's behalf. The handle binds
// both identifiers the five terminal methods need, and release is one-way.
func ExampleAgentSession_CreateTerminal() {
	ctx := context.Background()

	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},
		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			terminal, _, err := session.CreateTerminal(ctx, &acp.CreateTerminalParams{
				Command: "go",
				Args:    []string{"test", "./..."},
				Cwd:     acp.OptValue("/work"),
			})
			if err != nil {
				return nil, err
			}
			// Release frees what the client holds, and the handle is spent
			// afterwards: every operation on it then reports ErrTerminalReleased,
			// because the identifier is the client's to hand out again.
			defer func() { _, _ = terminal.Release(ctx, nil) }()

			if _, err = terminal.WaitForExit(ctx, nil); err != nil {
				return nil, err
			}
			// Exiting is not releasing, so the output is still there to read.
			output, err := terminal.Output(ctx, nil)
			if err != nil {
				return nil, err
			}
			status, _ := output.ExitStatus.Get()
			code, _ := status.ExitCode.Get()
			fmt.Printf("exit %d\n%s", code, output.Output)

			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		panic(err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return nil, errors.New("this example asks for nothing")
		},

		// All five or none: clientCapabilities.terminal is one boolean covering
		// every terminal method, so a client with four of them would refuse a call
		// it had advertised. A real client runs the command; this one pretends.
		Terminal: &acp.TerminalHandlers{
			Create: func(_ context.Context, request *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				fmt.Println("running:", request.Command, strings.Join(request.Args, " "))
				return &acp.CreateTerminalResponse{TerminalID: "terminal-1"}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{ExitCode: acp.OptValue[uint32](0)}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{
					Output:     "PASS\nok  example.com/work  0.2s\n",
					ExitStatus: acp.OptValue(acp.TerminalExitStatus{ExitCode: acp.OptValue[uint32](0)}),
				}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(_ context.Context, request *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				fmt.Println("released:", request.TerminalID)
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		panic(err)
	}
	if _, err = session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "run the tests"}},
	}); err != nil {
		panic(err)
	}

	// Output:
	// running: go test ./...
	// exit 0
	// PASS
	// ok  example.com/work  0.2s
	// released: terminal-1
}

// Extension methods carry what two implementations agree on privately. The
// underscore is the protocol's: every other name is reserved, and a method the
// specification defines is refused here because it has a typed path of its own.
func ExampleClientConn_Call() {
	ctx := context.Background()

	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},

		// Every extension request arrives here, undecoded: the grammar of one is
		// between the two implementations that agreed on it.
		CallFallback: func(_ context.Context, request *acp.ExtRequest) (json.RawMessage, error) {
			fmt.Println("agent served:", request.Method, string(request.Params))
			return json.RawMessage(`{"models":["haiku","opus"]}`), nil
		},
	})
	if err != nil {
		panic(err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return nil, errors.New("this example asks for nothing")
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	var result struct {
		Models []string `json:"models"`
	}
	if err = conn.Call(ctx, "_example.com/models", map[string]string{"for": "coding"}, &result); err != nil {
		panic(err)
	}
	fmt.Println("client got:", result.Models)

	// Output:
	// agent served: _example.com/models {"for":"coding"}
	// client got: [haiku opus]
}

// The protocol has three states for an optional property, and they mean different
// things: absent, explicitly null, and carrying a value.
func ExampleOpt() {
	type listRequest struct {
		Cursor acp.Opt[string] `json:"cursor,omitzero"`
	}

	for _, request := range []listRequest{
		{},
		{Cursor: acp.OptNull[string]()},
		{Cursor: acp.OptValue("page-2")},
	} {
		encoded, err := json.Marshal(request)
		if err != nil {
			panic(err)
		}
		value, present := request.Cursor.Get()
		fmt.Printf("%-20s present=%-5t null=%-5t value=%q\n",
			encoded, present, request.Cursor.IsNull(), value)
	}

	// Output:
	// {}                   present=false null=false value=""
	// {"cursor":null}      present=false null=true  value=""
	// {"cursor":"page-2"}  present=true  null=false value="page-2"
}

// _meta is the one place either peer may attach data the specification says
// nothing about. It is on every message that has the property, including the
// handshake.
func ExampleMeta() {
	meta, err := acp.NewMeta(map[string]any{"example.com/trace": "abc123"})
	if err != nil {
		panic(err)
	}

	var trace string
	present, err := meta.Decode("example.com/trace", &trace)
	if err != nil {
		panic(err)
	}
	fmt.Println("present:", present, "trace:", trace)

	params := &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "hello"}},
		Meta:   acp.OptValue(meta),
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(encoded))

	// Output:
	// present: true trace: abc123
	// {"prompt":[{"type":"text","text":"hello"}],"_meta":{"example.com/trace":"abc123"}}
}

// Elicitation is the agent asking the user for structured input. The client
// renders it; the agent names the mode and never the scope, because the operation
// it called has already decided one.
func ExampleAgentSession_CreateElicitation() {
	ctx := context.Background()

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {},
		RequestPermission: func(
			context.Context,
			*acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			return nil, errors.New("this example asks for nothing")
		},

		// Setting Form advertises clientCapabilities.elicitation.form and nothing
		// else. An agent asking for a URL elicitation is refused before it is sent,
		// because this client never said it could show a page.
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context,
				request *acp.CreateElicitationRequest,
				mode *acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				fmt.Println("asking:", request.Message)
				if scope, ok := mode.Value.(*acp.ElicitationFormModeSession); ok {
					fmt.Println("within session:", scope.SessionID)
				}
				for name := range mode.RequestedSchema.Properties {
					fmt.Println("field:", name)
				}
				// A real client renders the schema and waits. The three actions are
				// accept, decline and cancel.
				answer := acp.ElicitationContentValueString("main")
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{
						Content: acp.OptValue(map[string]acp.ElicitationContentValue{
							"branch": &answer,
						}),
					},
				}, nil
			},
		},
	})
	if err != nil {
		panic(err)
	}

	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session-1"}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			// No scope here: this session is the scope, and CreateElicitation fills
			// it in. ToolCallID would tie it to one tool call within the session.
			answer, elicitErr := session.CreateElicitation(ctx, &acp.CreateElicitationParams{
				Message: "which branch should I push to?",
				Mode: &acp.ElicitationFormMode{
					RequestedSchema: acp.ElicitationSchema{
						Type: acp.ElicitationSchemaTypeObject,
						Properties: map[string]acp.ElicitationPropertySchema{
							"branch": &acp.StringPropertySchema{},
						},
					},
				},
			})
			if elicitErr != nil {
				return nil, elicitErr
			}
			if accepted, ok := answer.Value.(*acp.ElicitationAcceptAction); ok {
				content, _ := accepted.Content.Get()
				if branch, ok := content["branch"].(*acp.ElicitationContentValueString); ok {
					fmt.Println("the user chose:", string(*branch))
				}
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		panic(err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	go func() { _ = agent.Run(ctx, agentSide) }()

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()

	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/work"})
	if err != nil {
		panic(err)
	}
	if _, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "push my work"}},
	}); err != nil {
		panic(err)
	}

	// Output:
	// asking: which branch should I push to?
	// within session: session-1
	// field: branch
	// the user chose: main
}
