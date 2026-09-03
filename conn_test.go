package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Tangerg/acp"
)

// A turn, end to end, over an in-memory pipe: initialize, session/new, a prompt
// whose agent streams updates and asks permission in the middle, and a stop
// reason.
//
// This is the shape the protocol exists for, and every piece of it is a different
// direction: the client calls initialize and prompt, the agent calls back for
// permission while the prompt is still outstanding, and the updates are
// notifications neither side answers. An implementation that got the directions
// wrong would pass a test of each half separately.
func TestATurnEndToEnd(t *testing.T) {
	var updates []string
	var permissionAsked int

	client, err := acp.NewClient(&acp.ClientConfig{
		Info: &acp.Implementation{Name: "an editor", Version: "1.0.0"},
		SessionUpdate: func(_ context.Context, notification *acp.SessionNotification) {
			if chunk, ok := notification.Update.(*acp.AgentMessageChunk); ok {
				if text, ok := chunk.Content.(*acp.TextContent); ok {
					updates = append(updates, text.Text)
				}
			}
		},
		RequestPermission: func(
			_ context.Context,
			request *acp.RequestPermissionRequest,
		) (*acp.RequestPermissionResponse, error) {
			permissionAsked++
			return &acp.RequestPermissionResponse{
				Outcome: &acp.SelectedPermissionOutcome{OptionID: request.Options[0].OptionID},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	agent, err := acp.NewAgent(&acp.AgentConfig{
		Info: &acp.Implementation{Name: "an agent", Version: "0.1.0"},
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(
			ctx context.Context,
			session *acp.AgentSession,
			_ *acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			if updateErr := session.Update(ctx, &acp.SessionUpdateParams{
				Update: &acp.AgentMessageChunk{
					ContentChunk: acp.ContentChunk{Content: &acp.TextContent{Text: "thinking"}},
				},
			}); updateErr != nil {
				return nil, updateErr
			}

			// A callback into the client while the prompt this is answering is
			// still outstanding. Both peers serve requests, and this is why.
			outcome, permissionErr := session.RequestPermission(ctx, &acp.RequestPermissionParams{
				ToolCall: acp.ToolCallUpdate{ToolCallID: "call-1"},
				Options: []acp.PermissionOption{
					{OptionID: "yes", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
				},
			})
			if permissionErr != nil {
				return nil, permissionErr
			}
			if _, ok := outcome.Outcome.(*acp.SelectedPermissionOutcome); !ok {
				return nil, errors.New("the client did not select an option")
			}

			if updateErr := session.Update(ctx, &acp.SessionUpdateParams{
				Update: &acp.AgentMessageChunk{
					ContentChunk: acp.ContentChunk{Content: &acp.TextContent{Text: "done"}},
				},
			}); updateErr != nil {
				return nil, updateErr
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	clientSide, agentSide := acp.NewInMemoryTransports()
	ctx := context.Background()

	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	defer agentConn.Close() //nolint:errcheck // Close is idempotent and the test closes explicitly below.

	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}

	// The handshake settled a version and exchanged capabilities, and both sides
	// see the same thing from their own direction.
	peer := conn.Peer()
	if peer.ProtocolVersion != acp.CurrentProtocolVersion {
		t.Errorf("negotiated version %d, want %d", peer.ProtocolVersion, acp.CurrentProtocolVersion)
	}
	if info, ok := peer.AgentInfo.Get(); !ok || info.Name != "an agent" {
		t.Errorf("the client does not know who the agent is: %+v", peer.AgentInfo)
	}
	if info, ok := agentConn.Peer().ClientInfo.Get(); !ok || info.Name != "an editor" {
		t.Errorf("the agent does not know who the client is: %+v", agentConn.Peer().ClientInfo)
	}

	session, created, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if created.SessionID != "sess-1" || session.ID() != "sess-1" {
		t.Fatalf("session id = %q / %q", created.SessionID, session.ID())
	}

	response, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{&acp.TextContent{Text: "do a thing"}},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Errorf("stop reason = %q, want end_turn", response.StopReason)
	}
	if permissionAsked != 1 {
		t.Errorf("permission was asked %d times, want 1", permissionAsked)
	}
	if strings.Join(updates, ",") != "thinking,done" {
		t.Errorf("updates = %v, want [thinking done]", updates)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Wait(); err != nil {
		t.Errorf("Wait after a local Close reported %v, want nil", err)
	}
	if err := agentConn.Wait(); err != nil {
		t.Errorf("the agent's Wait reported %v, want nil after a clean end of stream", err)
	}
}

// A method the peer never advertised is refused rather than served. Capabilities
// are an authority boundary: the agent was told the client could not read files.
func TestAnUnadvertisedMethodIsRefused(t *testing.T) {
	reached := false
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		// No ReadTextFile handler, so fs.readTextFile is not advertised. The
		// capability derived from the handlers is what goes on the wire.
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var readErr error
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		reached = true
		_, readErr = session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/w/a"})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !reached {
		t.Fatal("the prompt handler did not run")
	}
	if readErr == nil {
		t.Fatal("the agent read a file from a client that never advertised the capability")
	}
	if !strings.Contains(readErr.Error(), "clientCapabilities.fs.readTextFile") {
		t.Errorf("the refusal does not name the missing capability: %v", readErr)
	}
}

// The protocol gates three request parameters as well as the methods carrying
// them, and puts all three obligations on the client: it must restrict prompt
// content to what the agent advertised, verify the agent's MCP capabilities
// before naming an http or sse server, and only send additionalDirectories to an
// agent that advertised them. So the refusal happens before anything is sent.
func TestParametersTheAgentNeverAdvertisedAreRefused(t *testing.T) {
	reached := false
	agent := testAgent(t, func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
		reached = true
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})
	session := connectAndOpen(t, testClient(t), agent)
	ctx := context.Background()

	_, err := session.Prompt(ctx, &acp.PromptParams{
		Prompt: []acp.ContentBlock{
			&acp.TextContent{Text: "look at this"},
			&acp.ImageContent{Data: "iVBORw0KGgo=", MimeType: "image/png"},
		},
	})
	if err == nil {
		t.Fatal("an image reached an agent that never advertised promptCapabilities.image")
	}
	if !strings.Contains(err.Error(), "agentCapabilities.promptCapabilities.image") {
		t.Errorf("the refusal does not name the missing capability: %v", err)
	}
	if reached {
		t.Error("the prompt was sent before it was refused")
	}

	conn := session.Conn()
	_, _, err = conn.NewSession(ctx, &acp.NewSessionRequest{
		Cwd:        "/w",
		McpServers: []acp.McpServer{&acp.McpServerHTTP{Name: "docs", URL: "https://example.com"}},
	})
	if err == nil || !strings.Contains(err.Error(), "agentCapabilities.mcpCapabilities.http") {
		t.Errorf("an http MCP server was not refused by name: %v", err)
	}

	_, _, err = conn.NewSession(ctx, &acp.NewSessionRequest{
		Cwd:                   "/w",
		AdditionalDirectories: []string{"/other"},
	})
	if err == nil ||
		!strings.Contains(err.Error(), "agentCapabilities.sessionCapabilities.additionalDirectories") {
		t.Errorf("additionalDirectories was not refused by name: %v", err)
	}
}

// The same content, to an agent that said it could read it. A gate that refused
// both ways would be a gate on the content rather than on the advertisement.
func TestPromptContentTheAgentAdvertisedIsSent(t *testing.T) {
	var blocks int
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(_ context.Context, _ *acp.AgentSession, request *acp.PromptRequest) (*acp.PromptResponse, error) {
			blocks = len(request.Prompt)
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		Capabilities: &acp.AgentCapabilities{
			PromptCapabilities: acp.PromptCapabilities{Image: true},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, testClient(t), agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{
		Prompt: []acp.ContentBlock{
			&acp.TextContent{Text: "look at this"},
			&acp.ImageContent{Data: "iVBORw0KGgo=", MimeType: "image/png"},
		},
	}); err != nil {
		t.Fatalf("Prompt with advertised image content: %v", err)
	}
	if blocks != 2 {
		t.Errorf("the agent received %d content blocks, want 2", blocks)
	}
}

// The extension boundary: an underscore-prefixed name outside the specification
// reaches the fallback; standard and protocol-reserved names never do.
func TestExtensionMethodsAndTheReservedSet(t *testing.T) {
	var seen string
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		CallFallback: func(_ context.Context, request *acp.ExtRequest) (json.RawMessage, error) {
			seen = request.Method
			return json.RawMessage(`{"answered":true}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var extErr, standardErr, futureErr error
	var result struct{ Answered bool }
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		extErr = session.Conn().Call(ctx, "_vendor.example/thing", map[string]int{"n": 1}, &result)
		standardErr = session.Conn().Call(ctx, "session/prompt", nil, nil)
		futureErr = session.Conn().Call(ctx, "future/method", nil, nil)
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if extErr != nil {
		t.Fatalf("the extension call failed: %v", extErr)
	}
	if seen != "_vendor.example/thing" {
		t.Errorf("the fallback saw %q", seen)
	}
	if !result.Answered {
		t.Error("the extension's result did not come back")
	}
	if standardErr == nil {
		t.Fatal("the extension API sent session/prompt, bypassing the typed codec and the gate")
	}
	if !strings.Contains(standardErr.Error(), "the extension API does not send it") {
		t.Errorf("the refusal does not say why: %v", standardErr)
	}
	if futureErr == nil || !strings.Contains(futureErr.Error(), "must begin with an underscore") {
		t.Errorf("the extension API did not preserve future ACP method names: %v", futureErr)
	}
}

// One prompt at a time per session, because a session/cancel carries only a
// session identifier and so cannot say which turn it means.
func TestASecondConcurrentPromptIsRefused(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	agent := testAgent(t, func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
		// Only the first turn blocks. The prompt after it has to be allowed
		// through, which is the other half of the rule.
		first.Do(func() {
			close(inFlight)
			<-release
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})
	session := connectAndOpen(t, testClient(t), agent)

	firstTurn := make(chan error, 1)
	go func() {
		_, err := session.Prompt(context.Background(), &acp.PromptParams{})
		firstTurn <- err
	}()

	// The agent's handler running is the signal that the first prompt is genuinely
	// in flight, rather than a sleep long enough to hope it is.
	<-inFlight

	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); !errors.Is(err, acp.ErrPromptInProgress) {
		t.Fatalf("the second prompt failed with %v, want ErrPromptInProgress", err)
	}

	close(release)
	if err := <-firstTurn; err != nil {
		t.Fatalf("the first prompt failed: %v", err)
	}

	// And the rule is per turn, not per session for ever: once the first turn has
	// ended the next prompt is allowed.
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("a prompt after the first turn ended failed: %v", err)
	}
}

// -- helpers -----------------------------------------------------------------
