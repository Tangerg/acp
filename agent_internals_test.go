package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// Invalid parameters establish no capability agreement. Once that error has
// settled, the same connection must accept a corrected request rather than
// falsely reporting that it is already initialized.
func TestARejectedInitializeCanBeRetried(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := stream.Write(ctx, rawCall(1, methodInitialize, `{"protocolVersion":"invalid"}`)); err != nil {
		t.Fatalf("write invalid initialize: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
		t.Fatalf("invalid initialize was answered %d (%s), want invalid params", code, code)
	}

	if err := stream.Write(ctx, rawCall(2, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write corrected initialize: %v", err)
	}
	message, err := stream.Read(ctx)
	if err != nil {
		t.Fatalf("read corrected initialize response: %v", err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("read a %T, want a response", message)
	}
	if response.Error != nil {
		t.Fatalf("corrected initialize failed: %v", response.Error)
	}
}

// A later initialize may already be in the inbound queue when the first attempt
// fails. Holding it behind the first response preserves wire order without
// confusing an invalid attempt with an accepted capability agreement.
func TestARejectedInitializePromotesTheNextQueuedAttempt(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, request := range []*jsonrpc.Request{
		rawCall(1, methodInitialize, `{"protocolVersion":"invalid"}`),
		rawCall(2, methodInitialize, `{"protocolVersion":1}`),
	} {
		if err := stream.Write(ctx, request); err != nil {
			t.Fatalf("write initialize: %v", err)
		}
	}

	responses := make(map[int64]*jsonrpc.Response, 2)
	for range 2 {
		message, err := stream.Read(ctx)
		if err != nil {
			t.Fatalf("read initialize response: %v", err)
		}
		response, ok := message.(*jsonrpc.Response)
		if !ok {
			t.Fatalf("read a %T, want a response", message)
		}
		id, ok := response.ID.Raw().(int64)
		if !ok {
			t.Fatalf("response identifier is a %T, want int64", response.ID.Raw())
		}
		responses[id] = response
	}

	if response := responses[1]; response == nil || response.Error == nil {
		t.Fatalf("the invalid attempt was not rejected: %#v", response)
	}
	if response := responses[2]; response == nil || response.Error != nil {
		t.Fatalf("the queued valid attempt did not establish the connection: %#v", response)
	}
}

// A failed response finishes after promoting its successor. Publication belongs
// to the attempt that accepted the agreement, or the old response could open
// outbound work before the successful initialize response is written.
func TestOnlyTheAcceptedInitializePublishesTheHandshake(t *testing.T) {
	handshake := newHandshake()
	failed := handshake.registerAttempt()
	accepted := handshake.registerAttempt()

	failed.settle()
	if err := accepted.await(context.Background()); err != nil {
		t.Fatalf("await promoted attempt: %v", err)
	}
	handshake.accept(PeerInfo{})
	failed.publish()
	select {
	case <-handshake.whenPublished():
		t.Fatal("the rejected attempt published its successor's agreement")
	default:
	}

	accepted.settle()
	accepted.publish()
	select {
	case <-handshake.whenPublished():
	default:
		t.Fatal("the accepted attempt did not publish its agreement")
	}
}

// Initialize is admitted in wire order even though request handlers run
// concurrently. Otherwise two back-to-back calls let the scheduler, rather than
// the protocol stream, choose which one establishes the connection.
func TestTheFirstInitializeOwnsNegotiation(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx := context.Background()
	for _, id := range []int{1, 2} {
		if err := stream.Write(ctx, rawCall(id, methodInitialize, `{"protocolVersion":1}`)); err != nil {
			t.Fatalf("write initialize %d: %v", id, err)
		}
	}

	responses := make(map[int64]*jsonrpc.Response, 2)
	for range 2 {
		message, err := stream.Read(ctx)
		if err != nil {
			t.Fatalf("read initialize response: %v", err)
		}
		response, ok := message.(*jsonrpc.Response)
		if !ok {
			t.Fatalf("read a %T, want a response", message)
		}
		id, ok := response.ID.Raw().(int64)
		if !ok {
			t.Fatalf("response identifier is a %T, want int64", response.ID.Raw())
		}
		responses[id] = response
	}

	if response := responses[1]; response == nil || response.Error != nil {
		t.Fatalf("the first initialize did not establish the connection: %#v", response)
	}
	response := responses[2]
	if response == nil || response.Error == nil {
		t.Fatalf("the second initialize was not refused: %#v", response)
	}
	var wire *jsonrpc2.WireError
	if !errors.As(response.Error, &wire) || wire.Code != int64(ErrorCodeInvalidRequest) {
		t.Fatalf("the second initialize returned %v, want invalid request", response.Error)
	}
}

// ACP keeps JSON-RPC's discouraged null identifier as a real RequestId arm. It
// must remain a call all the way through dispatch and come back on the response,
// not collapse into the zero ID used by notifications.
func TestANullRequestIDIsAnsweredAsNull(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	request := &jsonrpc.Request{
		ID:     jsonrpc2.NullID(),
		Method: methodInitialize,
		Params: json.RawMessage(`{"protocolVersion":1}`),
	}
	if err := stream.Write(context.Background(), request); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	message, err := stream.Read(context.Background())
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("read a %T, want a response", message)
	}
	if !response.ID.IsValid() || response.ID.Raw() != nil {
		t.Fatalf("the null request ID became %#v", response.ID)
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

// Names without the extension prefix are reserved for later ACP revisions. A
// fallback that claimed one today would turn a future standard notification or
// request into a private dialect before this package could apply its typed path.
func TestAReservedFutureMethodDoesNotReachTheExtensionFallback(t *testing.T) {
	reached := false
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
		CallFallback: func(context.Context, *ExtRequest) (json.RawMessage, error) {
			reached = true
			return json.RawMessage(`{}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx := context.Background()
	if err := stream.Write(ctx, rawCall(1, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	if err := stream.Write(ctx, rawCall(2, "future/method", `{}`)); err != nil {
		t.Fatalf("write future method: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
		t.Fatalf("the reserved method was answered %d (%s), want method not found", code, code)
	}
	if reached {
		t.Fatal("a protocol-reserved method reached the extension fallback")
	}
}

// Acting on $/cancel_request narrows the legal answer to a valid result or
// -32800. A handler's context error is an implementation detail and must not be
// exposed as the generic -32603 response.
func TestACancelledHandlerIsAnsweredWithRequestCancelled(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	if err := stream.Write(context.Background(), rawCall(1, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := stream.Read(context.Background()); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}

	if err := stream.Write(context.Background(), rawCall(2, methodSessionPrompt,
		`{"sessionId":"sess-1","prompt":[]}`)); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := stream.Write(context.Background(), rawNotification(methodCancelRequest,
		`{"requestId":2}`)); err != nil {
		t.Fatalf("write cancellation: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeRequestCancelled {
		t.Fatalf("the cancelled prompt was answered %d (%s), want request cancelled", code, code)
	}
}

func TestReusingAnActiveRequestIDEndsTheConnection(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			<-ctx.Done()
			return &PromptResponse{StopReason: StopReasonCancelled}, nil
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, agentConn := connectRawAgent(t, agent)
	if err := stream.Write(context.Background(), rawCall(1, methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := stream.Read(context.Background()); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}

	if err := stream.Write(context.Background(), rawCall(2, methodSessionPrompt,
		`{"sessionId":"sess-1","prompt":[]}`)); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := stream.Write(context.Background(), rawCall(2, methodSessionNew,
		`{"cwd":"/w","mcpServers":[]}`)); err != nil {
		t.Fatalf("write duplicate id: %v", err)
	}
	if err := agentConn.Wait(); err == nil || !strings.Contains(err.Error(), "reused active request id") {
		t.Fatalf("Wait reported %v, want the duplicate request identifier failure", err)
	}
}

func rawAgentConnection(t *testing.T) (Connection, *AgentConn) {
	t.Helper()
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
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

	return connectRawAgent(t, agent)
}
