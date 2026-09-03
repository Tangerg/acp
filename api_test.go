package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// The rest of the public surface, exercised from a caller's side: the operations a
// connection and a session offer, and the two peers' own lifecycles.

// Authentication is control flow rather than failure. An agent that requires it
// answers session/new with -32000, the client authenticates, and the retry
// succeeds.
func TestAuthenticationIsControlFlow(t *testing.T) {
	authenticated := false
	agent, err := acp.NewAgent(&acp.AgentConfig{
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"}},
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			if !authenticated {
				return nil, &acp.Error{
					Code:    acp.ErrorCodeAuthenticationRequired,
					Message: "sign in first",
				}
			}
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Authenticate: func(
			_ context.Context,
			_ *acp.AgentConn,
			request *acp.AuthenticateRequest,
		) (*acp.AuthenticateResponse, error) {
			if request.MethodID != "oauth" {
				return nil, errors.New("unknown method")
			}
			authenticated = true
			return &acp.AuthenticateResponse{}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
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
	defer agentConn.Close() //nolint:errcheck // idempotent.

	conn, err := testClient(t).Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	defer conn.Close() //nolint:errcheck // idempotent.

	// The agent said what it accepts, which is how a client knows what to offer.
	methods := conn.Peer().AuthMethods
	if len(methods) != 1 {
		t.Fatalf("initialize advertised %d authentication methods, want one", len(methods))
	}
	method, ok := methods[0].(*acp.AuthMethodAgent)
	if !ok || method.ID != "oauth" {
		t.Fatalf("initialize advertised %#v, want the oauth agent method", methods[0])
	}

	_, _, err = conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"})
	if !errors.Is(err, acp.ErrAuthRequired) {
		t.Fatalf("NewSession returned %v, want a match for ErrAuthRequired", err)
	}

	if _, err := conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "oauth"}); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if _, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w"}); err != nil {
		t.Fatalf("NewSession after authenticating: %v", err)
	}
}

// Loading a session is gated on the agent advertising it, and the handler that
// serves it is what advertises it. Setting a mode is gated by data instead: an
// agent offers modes by returning them.
func TestLoadSessionAndSetMode(t *testing.T) {
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{
				SessionID: "sess-1",
				Modes: acp.OptValue(acp.SessionModeState{
					CurrentModeID:  "ask",
					AvailableModes: []acp.SessionMode{{ID: "ask", Name: "Ask first"}, {ID: "code", Name: "Code"}},
				}),
			}, nil
		},
		LoadSession: func(
			_ context.Context,
			session *acp.AgentSession,
			request *acp.LoadSessionRequest,
		) (*acp.LoadSessionResponse, error) {
			if session.ID() != request.SessionID {
				return nil, errors.New("the handle and the request disagree")
			}
			return &acp.LoadSessionResponse{}, nil
		},
		SetMode: func(
			_ context.Context,
			_ *acp.AgentSession,
			request *acp.SetSessionModeRequest,
		) (*acp.SetSessionModeResponse, error) {
			if request.ModeID != "code" {
				return nil, errors.New("unknown mode")
			}
			return &acp.SetSessionModeResponse{}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, testClient(t), agent)
	conn := session.Conn()
	ctx := context.Background()

	// The handler is what advertises the capability, so this agent's peer info
	// says so and the call is allowed through.
	if !conn.Peer().AgentCapabilities.LoadSession {
		t.Fatal("the agent has a LoadSession handler and did not advertise loadSession")
	}
	loaded, _, err := conn.LoadSession(ctx, &acp.LoadSessionRequest{SessionID: "sess-1", Cwd: "/w"})
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.ID() != "sess-1" {
		t.Errorf("the loaded handle names %q", loaded.ID())
	}
	if _, _, err := conn.LoadSession(ctx, nil); err == nil {
		t.Error("LoadSession with no params succeeded, and the session to load is in them")
	}

	if _, setErr := session.SetMode(ctx, &acp.SetModeParams{ModeID: "code"}); setErr != nil {
		t.Fatalf("SetMode: %v", setErr)
	}
	if _, setErr := session.SetMode(ctx, &acp.SetModeParams{ModeID: "telepathy"}); setErr == nil {
		t.Error("SetMode accepted a mode the agent does not have")
	}
}

// An agent that does not implement session/load does not advertise it, and the
// call is refused locally rather than sent: the peer's answer would be the same,
// and asking wastes a round trip.
func TestLoadSessionIsRefusedWithoutTheCapability(t *testing.T) {
	session := connectAndOpen(t, testClient(t), testAgent(t, nil))
	_, _, err := session.Conn().LoadSession(context.Background(),
		&acp.LoadSessionRequest{SessionID: "sess-1", Cwd: "/w"})
	if err == nil {
		t.Fatal("LoadSession succeeded on an agent that does not implement it")
	}
	if !strings.Contains(err.Error(), "agentCapabilities.loadSession") {
		t.Errorf("the refusal does not name the missing capability: %v", err)
	}
}

// Extension notifications, in both directions, and the reserved set applies to
// them too.
func TestExtensionNotifications(t *testing.T) {
	clientHeard := make(chan string, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		NotifyFallback: func(_ context.Context, notification *acp.ExtNotification) {
			clientHeard <- notification.Method
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	agentHeard := make(chan string, 1)
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		NotifyFallback: func(_ context.Context, notification *acp.ExtNotification) {
			agentHeard <- notification.Method
		},
		Prompt: func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			if notifyErr := session.Conn().Notify(ctx, "_vendor.example/ping", map[string]int{"n": 1}); notifyErr != nil {
				return nil, notifyErr
			}
			if reserved := session.Conn().Notify(ctx, "session/cancel", nil); reserved == nil {
				return nil, errors.New("the extension API sent a standard notification")
			}
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, client, agent)
	ctx := context.Background()
	if _, err := session.Prompt(ctx, &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if heard := <-clientHeard; heard != "_vendor.example/ping" {
		t.Errorf("the client heard %q", heard)
	}

	// And the other direction.
	if err := session.Conn().Notify(ctx, "_vendor.example/pong", nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if heard := <-agentHeard; heard != "_vendor.example/pong" {
		t.Errorf("the agent heard %q", heard)
	}
	if err := session.Conn().Notify(ctx, "session/update", nil); err == nil {
		t.Error("the extension API sent session/update")
	}
	if err := session.Conn().Call(ctx, "initialize", nil, nil); err == nil {
		t.Error("the extension API sent initialize")
	}
	if err := session.Conn().Notify(ctx, "future/notification", nil); err == nil {
		t.Error("the extension API sent a name reserved for a future ACP notification")
	}
}

// An agent's Call reaches a client with no fallback handler, and gets
// method-not-found: the peer asked for something this side does not implement.
func TestAnExtensionWithNoFallbackIsNotFound(t *testing.T) {
	var reply error
	agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
		reply = session.Conn().Call(ctx, "_vendor.example/thing", nil, &json.RawMessage{})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})
	session := connectAndOpen(t, testClient(t), agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var failure *acp.Error
	if !errors.As(reply, &failure) {
		t.Fatalf("the reply is a %T, want *acp.Error", reply)
	}
	if failure.Code != acp.ErrorCodeMethodNotFound {
		t.Errorf("code = %s, want method not found", failure.Code)
	}
}

// Conns lists what is open and forgets what is not, on both sides.
func TestConnsListsWhatIsOpen(t *testing.T) {
	client := testClient(t)
	agent := testAgent(t, nil)
	clientSide, agentSide := acp.NewInMemoryTransports()
	ctx := context.Background()

	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}

	if got := count(client.Conns()); got != 1 {
		t.Errorf("the client lists %d connections, want 1", got)
	}
	if got := count(agent.Conns()); got != 1 {
		t.Errorf("the agent lists %d connections, want 1", got)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := agentConn.Wait(); err != nil {
		t.Fatalf("the agent's Wait reported %v", err)
	}

	if got := count(client.Conns()); got != 0 {
		t.Errorf("the client still lists %d connections after closing", got)
	}
	if got := count(agent.Conns()); got != 0 {
		t.Errorf("the agent still lists %d connections after the peer hung up", got)
	}
}

// Run is Connect, then Wait, then Close — the one place a context owns a
// connection's lifetime, and it says so.
func TestRunEndsWithItsContext(t *testing.T) {
	agent := testAgent(t, nil)
	clientSide, agentSide := acp.NewInMemoryTransports()

	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- agent.Run(ctx, agentSide) }()

	conn, err := testClient(t).Connect(context.Background(), clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}

	cancel()
	if runErr := <-ran; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", runErr)
	}

	// And the client sees the connection end, because Run closed it.
	if waitErr := conn.Wait(); waitErr != nil {
		t.Errorf("the client's Wait reported %v, want nil after a clean end of stream", waitErr)
	}
}

// Run also returns when the peer hangs up, without waiting for its context.
func TestRunEndsWhenThePeerHangsUp(t *testing.T) {
	agent := testAgent(t, nil)
	clientSide, agentSide := acp.NewInMemoryTransports()

	ran := make(chan error, 1)
	go func() { ran <- agent.Run(context.Background(), agentSide) }()

	conn, err := testClient(t).Connect(context.Background(), clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-ran; err != nil {
		t.Fatalf("Run returned %v, want nil after a clean end of stream", err)
	}
}

// A transport is connected at most once, and saying so is cheaper than debugging
// two connections sharing one pipe.
func TestATransportIsConnectedOnce(t *testing.T) {
	clientSide, _ := acp.NewInMemoryTransports()
	ctx := context.Background()

	stream, err := clientSide.Connect(ctx)
	if err != nil {
		t.Fatalf("the first Connect failed: %v", err)
	}
	defer stream.Close() //nolint:errcheck // idempotent.

	if _, err := clientSide.Connect(ctx); err == nil {
		t.Fatal("a transport was connected twice")
	}
}

func count[T any](seq func(func(T) bool)) int {
	total := 0
	for range seq {
		total++
	}
	return total
}
