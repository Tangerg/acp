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

// Two rules a well-behaved client cannot be made to break, so they are tested by
// speaking to the connection directly. This is an internal test because minting a
// request identifier is not something the public API does — a caller names a
// method by calling the operation for it — and the identifier type is deliberately
// not constructible from outside.

// A connection refuses work before initialize, because serving it would mean
// serving it under capabilities nobody has exchanged.
func TestAnAgentRefusesWorkBeforeInitialize(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	call := rawCall(1, methodSessionNew, `{"cwd":"/w","mcpServers":[]}`)
	if err := stream.Write(context.Background(), call); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("code = %d (%s), want invalid request", code, code)
	}
}

// A second initialize is not a re-negotiation. The capabilities already gate work
// in flight, and letting them change under it would make the authority boundary
// depend on timing.
func TestASecondInitializeIsRefused(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx := context.Background()
	if err := stream.Write(ctx, rawCall(1, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the first answer: %v", err)
	}

	if err := stream.Write(ctx, rawCall(2, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("the second initialize was answered %d (%s), want invalid request", code, code)
	}
}

// An unknown method is method-not-found, and an extension method with no fallback
// handler is the same: the peer asked for something this side does not implement.
func TestAnUnimplementedMethodIsNotFound(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx := context.Background()
	if err := stream.Write(ctx, rawCall(1, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read: %v", err)
	}

	for id, method := range map[int]string{2: methodLogout, 3: "_vendor.example/thing"} {
		if err := stream.Write(ctx, rawCall(id, method, `{}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
			t.Fatalf("%s was answered %d (%s), want method not found", method, code, code)
		}
	}
}

func rawAgentConnection(t *testing.T) (Connection, *AgentConn) {
	t.Helper()
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

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

func rawCall(id int, method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{
		ID:     jsonrpc2.Int64ID(int64(id)),
		Method: method,
		Params: json.RawMessage(params),
	}
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
	return errorCodeOf(wire.Code)
}
