package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

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

func newTestAgentConnWith(limits Limits) *AgentConn {
	conn := &AgentConn{connection: newConnection()}
	conn.link = newLink(nil, conn, nil, limits)
	return conn
}

func rawCall(id int, method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{
		ID:     jsonrpc2.Int64ID(int64(id)),
		Method: method,
		Params: json.RawMessage(params),
	}
}

func rawNotification(method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{Method: method, Params: json.RawMessage(params)}
}

func readRawErrorCode(t *testing.T, stream Connection) ErrorCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	var wire *jsonrpc2.WireError
	if !errors.As(response.Error, &wire) {
		t.Fatalf("the response error is a %T", response.Error)
	}
	var failure *Error
	if err := errorFromWire(wire); !errors.As(err, &failure) {
		t.Fatalf("the wire error could not be represented as ACP: %v", err)
	}
	return failure.Code
}
