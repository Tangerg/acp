package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp"
)

// What the handshake settles, and when each side may act on it.
//
// Initialization is two moments on each side rather than one flag. A client may
// not serve anything until it has accepted the answer; an agent may not send
// anything until it has written one. Between those moments each side knows
// something the other does not, and acting on it is acting alone.

// A client serves nothing until it has accepted the initialize answer.
//
// Its read loop starts before the handshake completes, because the answer arrives
// on it. Everything else that arrives first used to reach application code — a
// session update handled, an extension fallback called — while Connect was still
// running and there was no negotiated peer to judge any of it against.
func TestAClientServesNothingBeforeItsHandshakeIsAnswered(t *testing.T) {
	updated := make(chan struct{}, 1)
	extended := make(chan struct{}, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {
			updated <- struct{}{}
		},
		RequestPermission: denyingPermission,
		CallFallback: func(context.Context, *acp.ExtRequest) (json.RawMessage, error) {
			extended <- struct{}{}
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	stream, connected, ctx := rawAgentPending(t, client)

	// The client's own initialize, taken off the wire and deliberately not
	// answered yet. This is the window the test is about.
	handshake := expectCall(ctx, t, stream, "initialize")

	// A baseline notification and an extension call, both before the answer.
	writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","method":"session/update","params":`+
		`{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk",`+
		`"content":{"type":"text","text":"too early"}}}}`)
	refused := roundTrip(ctx, t, stream, 500, "_vendor.example/thing", `{}`)
	if refused.Error == nil {
		t.Fatal("an extension call was served before the client had accepted the handshake")
	}
	if !strings.Contains(refused.Error.Error(), "before initialize") {
		t.Errorf("refused with %v, which does not say why", refused.Error)
	}

	answerCall(ctx, t, stream, handshake, fmt.Sprintf(`{"protocolVersion":%d}`, acp.CurrentProtocolVersion))
	result := <-connected
	if result.err != nil {
		t.Fatalf("Client.Connect: %v", result.err)
	}
	t.Cleanup(func() { _ = result.conn.Close() })

	select {
	case <-updated:
		t.Fatal("a session update reached the application before there was a negotiated peer")
	case <-extended:
		t.Fatal("an extension call reached the application before there was a negotiated peer")
	default:
	}
}

// What the agent may advertise depends on the client in front of it.
//
// Two fields of the initialize response are the connection's rather than the
// configuration's, and both were copied straight out of the configuration. The
// schema says a terminal authentication method may be advertised "only when the
// client enabled its terminal authentication capability", and that the position
// encoding is "selected by the agent from the client's supported encodings".
func TestTheInitializeAnswerIsBuiltForTheClientInFrontOfIt(t *testing.T) {
	terminal := &acp.AuthMethodTerminal{ID: "tui", Name: "Sign in with a terminal"}
	byAgent := &acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"}

	tests := map[string]struct {
		methods  []acp.AuthMethod
		optedIn  bool
		expected []string
	}{
		"terminal only, opted in":   {[]acp.AuthMethod{terminal}, true, []string{"tui"}},
		"terminal only, opted out":  {[]acp.AuthMethod{terminal}, false, nil},
		"agent only, opted in":      {[]acp.AuthMethod{byAgent}, true, []string{"oauth"}},
		"agent only, opted out":     {[]acp.AuthMethod{byAgent}, false, []string{"oauth"}},
		"mixed, opted in":           {[]acp.AuthMethod{terminal, byAgent}, true, []string{"tui", "oauth"}},
		"mixed, opted out":          {[]acp.AuthMethod{terminal, byAgent}, false, []string{"oauth"}},
		"none configured, opted in": {nil, true, nil},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			agent, err := acp.NewAgent(&acp.AgentConfig{
				AuthMethods: test.methods,
				Authenticate: func(context.Context, *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
					return &acp.AuthenticateResponse{}, nil
				},
				NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
					return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
				},
				Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				},
				Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			_, stream, ctx := rawClientFor(t, agent)

			answered := answerInitializeRaw(ctx, t, stream,
				`{"protocolVersion":1,"clientCapabilities":{"auth":{"terminal":`+
					boolText(test.optedIn)+`}}}`)

			offered := make([]string, 0, len(answered.AuthMethods))
			for _, method := range answered.AuthMethods {
				switch method := method.(type) {
				case *acp.AuthMethodTerminal:
					offered = append(offered, string(method.ID))
				case *acp.AuthMethodAgent:
					offered = append(offered, string(method.ID))
				}
			}
			if len(offered) == 0 {
				offered = nil
			}
			if !equalStrings(offered, test.expected) {
				t.Fatalf("advertised %v, want %v", offered, test.expected)
			}
		})
	}
}

// The position encoding the agent answers with is one the client offered, or
// none.
//
// An encoding the client never offered is not a selection. Sending one anyway
// leaves the two peers counting character offsets differently, which is a
// disagreement about what every position in the conversation means.
func TestThePositionEncodingIsChosenFromWhatTheClientOffered(t *testing.T) {
	tests := map[string]struct {
		configured acp.Opt[acp.PositionEncodingKind]
		offered    string
		expected   acp.Opt[acp.PositionEncodingKind]
	}{
		"an encoding the client offered": {
			configured: acp.OptValue(acp.PositionEncodingKindUtf8),
			offered:    `["utf-8","utf-16"]`,
			expected:   acp.OptValue(acp.PositionEncodingKindUtf8),
		},
		"an encoding the client did not offer": {
			configured: acp.OptValue(acp.PositionEncodingKindUtf8),
			offered:    `["utf-16"]`,
		},
		"a client that offered none": {
			configured: acp.OptValue(acp.PositionEncodingKindUtf8),
			offered:    `[]`,
		},
		"an agent that configured none": {
			offered: `["utf-8","utf-16"]`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			agent, err := acp.NewAgent(&acp.AgentConfig{
				NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
					return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
				},
				Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				},
				Cancel:       func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
				Capabilities: &acp.AgentCapabilities{PositionEncoding: test.configured},
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			conn, stream, ctx := rawClientFor(t, agent)

			answered := answerInitializeRaw(ctx, t, stream,
				`{"protocolVersion":1,"clientCapabilities":{"positionEncodings":`+test.offered+`}}`)

			got, selected := answered.AgentCapabilities.PositionEncoding.Get()
			want, wanted := test.expected.Get()
			if selected != wanted || got != want {
				t.Fatalf("answered with (%q, %t), want (%q, %t)", got, selected, want, wanted)
			}
			// And the connection's own snapshot says the same thing.
			if peer := conn.Peer().AgentCapabilities.PositionEncoding; peer != test.expected {
				t.Errorf("the connection reports %v", peer)
			}
		})
	}
}

// A client refuses an encoding it never offered rather than proceed under two
// readings of the same offsets.
func TestAClientRefusesAPositionEncodingItDidNotOffer(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Capabilities: &acp.ClientCapabilities{
			PositionEncodings: []acp.PositionEncodingKind{acp.PositionEncodingKindUtf16},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	stream, connected, ctx := rawAgentPending(t, client)
	request := readRaw(ctx, t, stream)
	writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","id":`+idOf(t, request)+
		`,"result":{"protocolVersion":1,"agentCapabilities":{"positionEncoding":"utf-8"}}}`)

	result := <-connected
	if result.err == nil {
		_ = result.conn.Close()
		t.Fatal("the client accepted an encoding it never offered")
	}
	if !strings.Contains(result.err.Error(), "position encoding") {
		t.Errorf("Connect failed with %v, which does not say what was wrong", result.err)
	}
}

// A terminal-only agent needs no Authenticate handler, and a client must not call
// authenticate with a terminal method.
//
// The schema puts both halves plainly: the client "MUST NOT pass this method to
// authenticate", because a terminal method is performed by running the agent
// again in an interactive terminal. Requiring the handler rejected a valid agent;
// letting the call through would have sent the agent a method it cannot serve.
func TestTerminalAuthenticationNeedsNoHandlerAndCannotBeCalled(t *testing.T) {
	agent, err := acp.NewAgent(&acp.AgentConfig{
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodTerminal{ID: "tui", Name: "Sign in with a terminal"}},
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent refused a terminal-only agent, which serves no authenticate call: %v", err)
	}

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Capabilities: &acp.ClientCapabilities{
			Auth: acp.AuthCapabilities{Terminal: true},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	session := connectAndOpen(t, client, agent)
	conn := session.Conn()

	// The client learns the method from the handshake, which is the only place it
	// could.
	if len(conn.Peer().AuthMethods) != 1 {
		t.Fatalf("the client was told about %d authentication methods", len(conn.Peer().AuthMethods))
	}

	_, err = conn.Authenticate(context.Background(), &acp.AuthenticateRequest{MethodID: "tui"})
	if err == nil {
		t.Fatal("the client called authenticate with a terminal method")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("refused with %v, which does not say why", err)
	}
}

// answerInitializeRaw performs the handshake from the hand-driven client side and
// returns what the agent answered.
func answerInitializeRaw(
	ctx context.Context,
	t *testing.T,
	stream acp.Connection,
	params string,
) *acp.InitializeResponse {
	t.Helper()

	response := roundTrip(ctx, t, stream, 1, "initialize", params)
	if response.Error != nil {
		t.Fatalf("initialize was refused: %v", response.Error)
	}
	answered := new(acp.InitializeResponse)
	if err := json.Unmarshal(response.Result, answered); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	return answered
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An agent waits for its client rather than refusing to send.
//
// Connect returns before a handshake arrives, so the only honest answers to "may
// I send yet" are "wait" and "the connection ended". A flag set after the
// initialize response was written answered "not yet" to handlers that were only
// running because the client already had that response — the client observes the
// answer during the write, and the request it sends next is served on a different
// goroutine from the one still finishing the handshake.
func TestAnAgentWaitsForTheHandshakeRatherThanRefusing(t *testing.T) {
	t.Run("a caller bounds its own wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, agentSide := acp.NewInMemoryTransports()
			conn, err := testAgent(t, nil).Connect(context.Background(), agentSide)
			if err != nil {
				t.Fatalf("Agent.Connect: %v", err)
			}
			defer conn.Close() //nolint:errcheck // idempotent.

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			// No client has connected, so this waits and is stopped by its own
			// deadline rather than told the connection is unusable.
			err = conn.Call(ctx, "_vendor.example/thing", nil, nil)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Call returned %v, want the caller's own deadline", err)
			}
		})
	})

	t.Run("a connection that ends releases the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, agentSide := acp.NewInMemoryTransports()
			conn, err := testAgent(t, nil).Connect(context.Background(), agentSide)
			if err != nil {
				t.Fatalf("Agent.Connect: %v", err)
			}

			waited := make(chan error, 1)
			go func() {
				waited <- conn.Notify(context.Background(), "_vendor.example/thing", nil)
			}()
			synctest.Wait()

			if err := conn.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := <-waited; !errors.Is(err, acp.ErrConnectionClosed) {
				t.Fatalf("the waiting call returned %v, want ErrConnectionClosed", err)
			}
		})
	})
}

// A client serves what the agent sent after answering, and refuses what came
// before.
//
// Those are two facts and the client used to have one. It published the
// negotiated peer on the goroutine that called Connect, which runs after the read
// loop has queued the answer and after the delivery loop has handed it over — so
// the agent's next message could be refused for arriving before a handshake that
// had already been answered. Publication is now part of ordered delivery, and
// whether a message preceded the answer is the read loop's to say.
func TestAClientServesWhatTheAgentSendsAfterAnswering(t *testing.T) {
	// Repeated, because the failure it guards against was a race between three
	// goroutines and one run proves little.
	for range 100 {
		heard := make(chan string, 4)
		client, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate: func(context.Context, *acp.SessionNotification) {
				heard <- "session/update"
			},
			RequestPermission: denyingPermission,
			CallFallback: func(_ context.Context, request *acp.ExtRequest) (json.RawMessage, error) {
				heard <- request.Method
				return json.RawMessage(`{}`), nil
			},
			NotifyFallback: func(_ context.Context, notification *acp.ExtNotification) {
				heard <- notification.Method
			},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}

		stream, connected, ctx := rawAgentPending(t, client)
		handshake := expectCall(ctx, t, stream, "initialize")

		// The answer, then a request and a notification with no gap: an agent may
		// send both the moment its answer is on the wire.
		answerCall(ctx, t, stream, handshake, fmt.Sprintf(`{"protocolVersion":%d}`, acp.CurrentProtocolVersion))
		writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","method":"_vendor.example/after","params":{}}`)
		answered := roundTrip(ctx, t, stream, 900, "_vendor.example/asked", `{}`)

		result := <-connected
		if result.err != nil {
			t.Fatalf("Client.Connect: %v", result.err)
		}
		if answered.Error != nil {
			t.Fatalf("an extension call sent after the answer was refused: %v", answered.Error)
		}

		reached := map[string]bool{}
		for range 2 {
			select {
			case method := <-heard:
				reached[method] = true
			case <-ctx.Done():
				t.Fatalf("only %v reached the application", reached)
			}
		}
		if !reached["_vendor.example/after"] || !reached["_vendor.example/asked"] {
			t.Fatalf("the application saw %v", reached)
		}
		if err := result.conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// The identifier passed to authenticate is one the handshake advertised.
//
// The schema says it "must be one of the methods advertised in the initialize
// response". The client refused a terminal method and sent an unknown one; the
// agent dispatched an unknown one straight to the application, which is what a
// peer that does not go through this package would do.
func TestAuthenticateIsHeldToTheAdvertisedMethods(t *testing.T) {
	advertised := []acp.AuthMethod{
		&acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"},
		&acp.AuthMethodTerminal{ID: "tui", Name: "Sign in with a terminal"},
	}

	tests := map[string]struct {
		methodID acp.AuthMethodID
		accepted bool
		says     string
	}{
		"an advertised agent method": {methodID: "oauth", accepted: true},
		"a terminal method":          {methodID: "tui", says: "terminal"},
		"a method nobody advertised": {methodID: "guess", says: "not one of the authentication methods"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			served := make(chan acp.AuthMethodID, 1)
			agent, err := acp.NewAgent(&acp.AgentConfig{
				AuthMethods: advertised,
				Authenticate: func(
					_ context.Context,
					request *acp.AuthenticateRequest,
				) (*acp.AuthenticateResponse, error) {
					served <- request.MethodID
					return &acp.AuthenticateResponse{}, nil
				},
				NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
					return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
				},
				Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				},
				Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			client, err := acp.NewClient(&acp.ClientConfig{
				SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
				RequestPermission: denyingPermission,
				Capabilities:      &acp.ClientCapabilities{Auth: acp.AuthCapabilities{Terminal: true}},
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			conn := connectAndOpen(t, client, agent).Conn()

			_, err = conn.Authenticate(context.Background(), &acp.AuthenticateRequest{MethodID: test.methodID})
			if test.accepted {
				if err != nil {
					t.Fatalf("Authenticate: %v", err)
				}
				if got := <-served; got != test.methodID {
					t.Fatalf("the agent served %q", got)
				}
				return
			}
			if err == nil {
				t.Fatal("the client sent an identifier the handshake did not offer")
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Errorf("refused with %v, which does not say why", err)
			}
			select {
			case got := <-served:
				t.Fatalf("the agent's handler was asked to authenticate %q", got)
			default:
			}
		})
	}
}

// The same rule on ingress, for a peer that does not go through this package's
// client.
func TestAnAgentRefusesAnUnadvertisedAuthenticationMethod(t *testing.T) {
	served := make(chan acp.AuthMethodID, 1)
	agent, err := acp.NewAgent(&acp.AgentConfig{
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodAgent{ID: "oauth", Name: "Sign in"}},
		Authenticate: func(_ context.Context, request *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			served <- request.MethodID
			return &acp.AuthenticateResponse{}, nil
		},
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, stream, ctx := rawClientFor(t, agent)
	initializeRaw(ctx, t, stream)

	refused := roundTrip(ctx, t, stream, 2, "authenticate", `{"methodId":"guess"}`)
	if refused.Error == nil {
		t.Fatal("an identifier this agent never advertised reached the application")
	}
	select {
	case got := <-served:
		t.Fatalf("the handler was asked to authenticate %q", got)
	default:
	}

	if accepted := roundTrip(ctx, t, stream, 3, "authenticate", `{"methodId":"oauth"}`); accepted.Error != nil {
		t.Fatalf("the advertised method was refused: %v", accepted.Error)
	}
	if got := <-served; got != "oauth" {
		t.Fatalf("the handler served %q", got)
	}
}

// Two authentication methods cannot share an identifier, because a client selects
// by it.
func TestDuplicateAuthenticationIdentifiersAreRefused(t *testing.T) {
	_, err := acp.NewAgent(&acp.AgentConfig{
		AuthMethods: []acp.AuthMethod{
			&acp.AuthMethodTerminal{ID: "same", Name: "Sign in with a terminal"},
			&acp.AuthMethodAgent{ID: "same", Name: "Sign in"},
		},
		Authenticate: func(context.Context, *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			return &acp.AuthenticateResponse{}, nil
		},
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err == nil {
		t.Fatal("an agent advertised two methods under one identifier, so the agent-handled one " +
			"could never be selected")
	}
	if !strings.Contains(err.Error(), "share the identifier") {
		t.Errorf("NewAgent failed with %v, which does not say why", err)
	}
}
