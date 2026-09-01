package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
