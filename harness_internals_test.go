package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

// deadlockGuard bounds a test that would otherwise hang on a lost answer. It is
// never the behaviour under test, so one generous value serves every test.
const deadlockGuard = 30 * time.Second

// The vocabulary the in-package tests are written in: driving one side of a
// connection at the wire, without the other side's implementation in the way. A
// helper belongs here once a second file needs it.

func connectRawAgent(t *testing.T, agent *Agent) (Connection, *AgentConn) {
	t.Helper()
	clientSide, agentSide := NewInMemoryTransports()
	ctx := context.Background()
	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	stream, err := clientSide.Connect(ctx)
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream, agentConn
}

// testSessionID is the session newSessionHandler opens. The raw params below
// name it, because a test that registers a prompt and then cancels a session
// needs both to name the same one.
const (
	testSessionID = "sess-1"
	promptParams  = `{"sessionId":"` + testSessionID + `","prompt":[]}`
	cancelParams  = `{"sessionId":"` + testSessionID + `"}`
)

// newTestAgentConnWith assembles an agent connection with no transport, for
// driving registration and turn state directly.
func newTestAgentConnWith(t *testing.T, limits Limits) *AgentConn {
	t.Helper()
	agent, err := NewAgent(&AgentConfig{NewSession: newSessionHandler, Prompt: endTurn, Limits: limits})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return newAgentConn(agent, nil)
}

// newTestClientConnWith assembles a client connection with no transport. The two
// handlers every client needs are filled in when the configuration leaves them
// unset.
func newTestClientConnWith(t *testing.T, config ClientConfig) *ClientConn {
	t.Helper()
	if config.SessionUpdate == nil {
		config.SessionUpdate = func(context.Context, *ClientConn, *SessionNotification) {}
	}
	if config.RequestPermission == nil {
		config.RequestPermission = func(context.Context, *ClientConn, *RequestPermissionRequest) (
			*RequestPermissionResponse, error,
		) {
			return cancelledPermission(), nil
		}
	}
	client, err := NewClient(&config)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return newClientConn(client, nil)
}

func rawCall(id int, method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{
		ID:     jsonrpc.Int64ID(int64(id)),
		Method: method,
		Params: json.RawMessage(params),
	}
}

func rawNotification(method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{Method: method, Params: json.RawMessage(params)}
}

func readRawErrorCode(t *testing.T, stream Connection) ErrorCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadlockGuard)
	defer cancel()

	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("read a %T, want a response", message)
	}
	if response.Error == nil {
		t.Fatal("the call succeeded, and it should not have")
	}
	var failure *Error
	if err := decodeResponse(response, nil); !errors.As(err, &failure) {
		t.Fatalf("the error could not be represented as ACP: %v", err)
	}
	return failure.Code
}

// errorCodeOf decodes the code of a response that carries an error.
func errorCodeOf(t *testing.T, response *jsonrpc.Response) ErrorCode {
	t.Helper()
	var failure *Error
	if err := decodeResponse(response, nil); !errors.As(err, &failure) {
		t.Fatalf("the response carries no ACP error: %v", err)
	}
	return failure.Code
}

// inboundFor is a call for an identifier, under a method that is served on a
// connection rather than within a session.
func inboundFor(id jsonrpc.ID) *jsonrpc.Request {
	return &jsonrpc.Request{ID: id, Method: methodAuthenticate}
}
