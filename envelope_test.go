package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// The envelope a message arrives in, checked before anything is done about it.
//
// The schema says of every standard method whether it expects a response, and
// JSON-RPC says a response carries a result or an error and exactly one of them.
// Both facts were recorded and neither was enforced, so a peer could name a method
// and then use it as the other kind — with the side effect happening either way.

// A notification method sent as a call is refused, and its handler does not run.
//
// It used to run and then be answered with an internal error, because a
// notification handler has no result to return: the update was delivered, the
// application acted on it, and the peer was told the agent had failed.
func TestANotificationMethodSentAsACallIsRefused(t *testing.T) {
	cancelled := make(chan acp.SessionID, 1)
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(_ context.Context, session *acp.AgentSession, _ *acp.CancelNotification) {
			cancelled <- session.ID()
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	_, stream, ctx := rawClientFor(t, agent)
	initializeRaw(ctx, t, stream)

	response := roundTrip(ctx, t, stream, 2, "session/cancel", `{"sessionId":"sess-1"}`)
	if response.Error == nil {
		t.Fatal("session/cancel was accepted as a call, so a notification handler ran and the peer " +
			"was then answered for a method that has no response")
	}
	if !strings.Contains(response.Error.Error(), "notification") {
		t.Errorf("the refusal was %v, which does not say the shape was the problem", response.Error)
	}

	select {
	case session := <-cancelled:
		t.Fatalf("the Cancel handler ran for session %q", session)
	default:
	}
}

// A request method sent as a notification is dropped, and its handler does not
// run.
//
// This is the direction that cannot be answered — there is no identifier to answer
// under — so dropping it is the whole of the remedy. Silence is not the worst
// outcome here: terminal/kill sent this way used to reach the handler, kill the
// terminal, and tell nobody, because a notification's result is discarded.
func TestARequestMethodSentAsANotificationIsDropped(t *testing.T) {
	killed := make(chan struct{}, 1)
	updated := make(chan struct{}, 1)
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate: func(context.Context, *acp.SessionNotification) {
			updated <- struct{}{}
		},
		RequestPermission: denyingPermission,
		Terminal: &acp.TerminalHandlers{
			Create: func(context.Context, *acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
				return &acp.CreateTerminalResponse{TerminalID: "term-1"}, nil
			},
			Output: func(context.Context, *acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
				return &acp.TerminalOutputResponse{}, nil
			},
			WaitForExit: func(
				context.Context,
				*acp.WaitForTerminalExitRequest,
			) (*acp.WaitForTerminalExitResponse, error) {
				return &acp.WaitForTerminalExitResponse{}, nil
			},
			Kill: func(context.Context, *acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
				killed <- struct{}{}
				return &acp.KillTerminalResponse{}, nil
			},
			Release: func(context.Context, *acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
				return &acp.ReleaseTerminalResponse{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, stream, ctx := rawAgentFor(t, client)

	writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","method":"terminal/kill",`+
		`"params":{"sessionId":"sess-1","terminalId":"term-1"}}`)
	// A real notification behind it, and both take the same path in the same
	// order. When this one has been handled, a kill that was going to be handled
	// already has been.
	writeRaw(ctx, t, stream, `{"jsonrpc":"2.0","method":"session/update","params":`+
		`{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk",`+
		`"content":{"type":"text","text":"hello"}}}}`)

	select {
	case <-updated:
	case <-ctx.Done():
		t.Fatal("the notification behind it was never handled")
	}
	select {
	case <-killed:
		t.Fatal("terminal/kill ran as a notification, so a terminal was killed and nobody was told")
	default:
	}
}

// A response must carry a result or an error, and exactly one of them.
func TestAMalformedResponseEnvelopeIsRefused(t *testing.T) {
	tests := map[string]struct {
		answer string
		accept bool
		says   string
	}{
		"an empty object is a result": {
			// Which is what a response type with only optional properties looks
			// like, and what the TypeScript agent answers authenticate with.
			answer: `"result":{}`,
			accept: true,
		},
		"neither a result nor an error": {
			// It used to be read as a zero-valued success for every result type in
			// the schema.
			answer: `"result_omitted":true`,
			says:   "neither a result nor an error",
		},
		"both a result and an error": {
			answer: `"result":{},"error":{"code":-32000,"message":"no"}`,
			says:   "both a result and an error",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			conn, stream, ctx := rawAgentFor(t, testClient(t))

			authenticated := make(chan error, 1)
			go func() {
				_, err := conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "oauth"})
				authenticated <- err
			}()

			request := readRaw(ctx, t, stream)
			writeRaw(ctx, t, stream, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`,
				idOf(t, request), test.answer))

			err := <-authenticated
			if test.accept {
				if err != nil {
					t.Fatalf("Authenticate reported %v for a valid empty result", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the malformed answer was accepted as a success")
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Errorf("Authenticate reported %v, which does not say %q", err, test.says)
			}
		})
	}
}

// rawClientFor puts a connected agent opposite a hand-driven stream, which is how
// a test says something about the wire that the typed client would not let it say.
func rawClientFor(t *testing.T, agent *acp.Agent) (*acp.AgentConn, acp.Connection, context.Context) {
	t.Helper()

	clientSide, agentSide := acp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := clientSide.Connect(ctx)
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return conn, stream, ctx
}

// rawAgentFor is the mirror: a connected client opposite a hand-driven stream,
// with the handshake already answered so that the test can start where it means
// to.
func rawAgentFor(t *testing.T, client *acp.Client) (*acp.ClientConn, acp.Connection, context.Context) {
	t.Helper()

	stream, connected, ctx := rawAgentPending(t, client)
	if err := answerHandshake(ctx, stream); err != nil {
		t.Fatalf("answering the handshake: %v", err)
	}
	result := <-connected
	if result.err != nil {
		t.Fatalf("Client.Connect: %v", result.err)
	}
	t.Cleanup(func() { _ = result.conn.Close() })
	return result.conn, stream, ctx
}

// rawAgentPending is rawAgentFor stopped one step earlier: the client is
// connecting and its handshake is unanswered, which is the only window in which a
// test can send it something it must refuse.
func rawAgentPending(
	t *testing.T,
	client *acp.Client,
) (acp.Connection, <-chan connectResult, context.Context) {
	t.Helper()

	clientSide, agentSide := acp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	stream, err := agentSide.Connect(ctx)
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	connected := make(chan connectResult, 1)
	go func() {
		conn, err := client.Connect(ctx, clientSide)
		connected <- connectResult{conn: conn, err: err}
	}()
	return stream, connected, ctx
}

// A connectResult carries what Connect returned, because the test drives the
// other end of the handshake and cannot simply call it.
type connectResult struct {
	conn *acp.ClientConn
	err  error
}

// answerHandshake plays the agent's one obligatory part, so that a test driving
// the wire by hand can start where it means to.
func answerHandshake(ctx context.Context, stream acp.Connection) error {
	opening, err := stream.Read(ctx)
	if err != nil {
		return err
	}
	request, ok := opening.(*jsonrpc.Request)
	if !ok || request.Method != "initialize" {
		return fmt.Errorf("the client opened with %v", opening)
	}
	answer, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
		ID:     request.ID,
		Result: []byte(fmt.Sprintf(`{"protocolVersion":%d}`, acp.CurrentProtocolVersion)),
	})
	if err != nil {
		return err
	}
	decoded, err := jsonrpc.DecodeMessage(answer)
	if err != nil {
		return err
	}
	return stream.Write(ctx, decoded)
}

func writeRaw(ctx context.Context, t *testing.T, stream acp.Connection, line string) {
	t.Helper()

	// Built from wire bytes rather than from a struct literal, because the jsonrpc
	// package deliberately does not mint request identifiers: a transport frames
	// and unframes, and a test standing where a transport stands does the same.
	message, err := jsonrpc.DecodeMessage([]byte(line))
	if err != nil {
		t.Fatalf("encoding %s: %v", line, err)
	}
	if err := stream.Write(ctx, message); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readRaw(ctx context.Context, t *testing.T, stream acp.Connection) jsonrpc.Message {
	t.Helper()

	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return message
}

// idOf recovers a message's identifier as the JSON it was written as, so that a
// hand-built answer can name the call it answers.
func idOf(t *testing.T, message jsonrpc.Message) string {
	t.Helper()

	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		t.Fatalf("encoding %v: %v", message, err)
	}
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decoding %s: %v", encoded, err)
	}
	if len(envelope.ID) == 0 {
		t.Fatalf("%s carries no identifier to answer", encoded)
	}
	return string(envelope.ID)
}

// initializeRaw performs the handshake from the hand-driven side, because an
// agent serves nothing until it has happened and a test about anything else
// should not be a test about that.
func initializeRaw(ctx context.Context, t *testing.T, stream acp.Connection) {
	t.Helper()

	params := fmt.Sprintf(`{"protocolVersion":%d,"clientCapabilities":{}}`, acp.CurrentProtocolVersion)
	if response := roundTrip(ctx, t, stream, 1, "initialize", params); response.Error != nil {
		t.Fatalf("initialize was refused: %v", response.Error)
	}
}

// roundTrip sends one hand-built request and reads the answer to it.
func roundTrip(
	ctx context.Context,
	t *testing.T,
	stream acp.Connection,
	id int64,
	method, params string,
) *jsonrpc.Response {
	t.Helper()

	writeRaw(ctx, t, stream, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`,
		id, method, params))
	message := readRaw(ctx, t, stream)
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("%s was answered with %T", method, message)
	}
	return response
}

// An error's data has three states on the wire, and all three survive the round
// trip in both directions.
//
// Opt exists to keep absent and null apart. The adapter between it and the
// JSON-RPC envelope used to collapse them: an outbound explicit null was omitted,
// because Get reports null as not present, and an inbound null came back as a
// present raw "null". A client relaying an agent's error would then send a
// different error from the one it was given.
func TestErrorDataKeepsItsThreeStatesInbound(t *testing.T) {
	tests := map[string]struct {
		sent    string // the data member of the error the peer sends, or "" for absent
		present bool
		null    bool
		want    string
	}{
		"absent": {sent: ""},
		"null":   {sent: `,"data":null`, null: true},
		"a scalar": {
			sent: `,"data":"a string"`, present: true, want: `"a string"`,
		},
		"an object": {
			sent: `,"data":{"retryAfter":30}`, present: true, want: `{"retryAfter":30}`,
		},
		"an array": {
			sent: `,"data":[1,2,3]`, present: true, want: `[1,2,3]`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			conn, stream, ctx := rawAgentFor(t, testClient(t))

			failed := make(chan error, 1)
			go func() {
				_, err := conn.Authenticate(ctx, &acp.AuthenticateRequest{MethodID: "oauth"})
				failed <- err
			}()

			request := readRaw(ctx, t, stream)
			writeRaw(ctx, t, stream, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"no"%s}}`,
				idOf(t, request), test.sent))

			var failure *acp.Error
			if err := <-failed; !errors.As(err, &failure) {
				t.Fatalf("Authenticate reported %v, want an *acp.Error", err)
			}

			data, present := failure.Data.Get()
			if present != test.present {
				t.Fatalf("Data.Get reported present=%v, want %v", present, test.present)
			}
			if got := failure.Data.IsNull(); got != test.null {
				t.Fatalf("Data.IsNull reported %v, want %v", got, test.null)
			}
			if present && string(data) != test.want {
				t.Fatalf("Data = %s, want %s", data, test.want)
			}
		})
	}
}

func TestErrorDataKeepsItsThreeStatesOutbound(t *testing.T) {
	tests := map[string]struct {
		data acp.Opt[json.RawMessage]
		want string // the error member the peer should read
	}{
		"absent": {
			want: `{"code":-32000,"message":"no"}`,
		},
		"null": {
			data: acp.OptNull[json.RawMessage](),
			want: `{"code":-32000,"message":"no","data":null}`,
		},
		"a scalar": {
			data: acp.OptValue(json.RawMessage(`"a string"`)),
			want: `{"code":-32000,"message":"no","data":"a string"}`,
		},
		"an object": {
			data: acp.OptValue(json.RawMessage(`{"retryAfter":30}`)),
			want: `{"code":-32000,"message":"no","data":{"retryAfter":30}}`,
		},
		"an array": {
			data: acp.OptValue(json.RawMessage(`[1,2,3]`)),
			want: `{"code":-32000,"message":"no","data":[1,2,3]}`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			agent, err := acp.NewAgent(&acp.AgentConfig{
				NewSession: func(context.Context, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
					return nil, &acp.Error{Code: -32000, Message: "no", Data: test.data}
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

			response := roundTrip(ctx, t, stream, 2, "session/new", `{"cwd":"/w","mcpServers":[]}`)
			if response.Error == nil {
				t.Fatal("session/new succeeded")
			}

			// Read the error back off the wire rather than off the struct: the
			// question is what the peer is sent.
			encoded, err := jsonrpc.EncodeMessage(response)
			if err != nil {
				t.Fatalf("encoding the answer: %v", err)
			}
			var envelope struct {
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatalf("decoding %s: %v", encoded, err)
			}
			if got := string(envelope.Error); got != test.want {
				t.Fatalf("the peer was sent %s, want %s", got, test.want)
			}
		})
	}
}
