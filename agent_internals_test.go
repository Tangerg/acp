package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

// Rules a well-behaved client cannot be made to break, tested by speaking to an
// agent's connection directly at the wire.

func readRawResponse(t *testing.T, stream Connection) *jsonrpc.Response {
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
	return response
}

func writeRaw(t *testing.T, stream Connection, request *jsonrpc.Request) {
	t.Helper()
	if err := stream.Write(context.Background(), request); err != nil {
		t.Fatalf("write %s: %v", request.Method, err)
	}
}

func initializeRawAgent(t *testing.T, stream Connection) {
	t.Helper()
	writeRaw(t, stream, rawCall(1, methodInitialize, `{"protocolVersion":1,"clientCapabilities":{}}`))
	if response := readRawResponse(t, stream); response.Error != nil {
		t.Fatalf("initialize failed: %s", response.Error)
	}
}

// A connection refuses work before initialize, because serving it would mean
// serving it under capabilities nobody has exchanged.
func TestAnAgentRefusesWorkBeforeInitialize(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	writeRaw(t, stream, rawCall(1, methodSessionNew, `{"cwd":"/w","mcpServers":[]}`))
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("code = %d (%s), want invalid request", code, code)
	}
}

// The schema lets initialize be sent again, and the reference implementation
// serves it: a later initialize renegotiates, and what it settles is what the
// connection gates against from then on.
func TestASecondInitializeRenegotiates(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	initializeRawAgent(t, stream)
	if agentConn.Negotiated().ClientCapabilities.Terminal {
		t.Fatal("the first agreement already has a terminal")
	}
	writeRaw(t, stream, rawCall(2, methodInitialize, `{"protocolVersion":1,"clientCapabilities":{"terminal":true}}`))
	if response := readRawResponse(t, stream); response.Error != nil {
		t.Fatalf("the second initialize was refused: %s", response.Error)
	}
	if !agentConn.Negotiated().ClientCapabilities.Terminal {
		t.Fatal("the second initialize did not replace the agreement")
	}
}

// Invalid parameters establish no agreement, so a corrected initialize queued
// right behind them succeeds on the same connection.
func TestARejectedInitializeCanBeRetried(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	writeRaw(t, stream, rawCall(1, methodInitialize, `{"protocolVersion":"invalid"}`))
	writeRaw(t, stream, rawCall(2, methodInitialize, `{"protocolVersion":1}`))
	responses := map[jsonrpc.ID]*jsonrpc.Response{}
	for range 2 {
		response := readRawResponse(t, stream)
		responses[response.ID] = response
	}
	if response := responses[jsonrpc.Int64ID(1)]; response == nil || errorCodeOf(t, response) != ErrorCodeInvalidParams {
		t.Fatalf("the invalid attempt was not refused with invalid params: %#v", response)
	}
	if response := responses[jsonrpc.Int64ID(2)]; response == nil || response.Error != nil {
		t.Fatalf("the corrected attempt did not establish the connection: %#v", response)
	}
}

// Initialize is decided in wire order, so a request sent right behind it without
// waiting for its answer is served under the agreement it settled.
func TestWorkPipelinedBehindInitializeIsServed(t *testing.T) {
	for range 50 {
		stream, agentConn := rawAgentConnection(t)
		writeRaw(t, stream, rawCall(1, methodInitialize, `{"protocolVersion":1}`))
		writeRaw(t, stream, rawCall(2, methodSessionNew, `{"cwd":"/w","mcpServers":[]}`))
		for range 2 {
			if response := readRawResponse(t, stream); response.Error != nil {
				t.Fatalf("request %s failed: %s", response.ID, response.Error)
			}
		}
		_ = agentConn.Close()
	}
}

// ACP keeps JSON-RPC's discouraged null identifier as a real RequestId arm: it
// stays a call through dispatch and comes back on the response.
func TestANullRequestIDIsAnsweredAsNull(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	writeRaw(t, stream, &jsonrpc.Request{
		ID: jsonrpc.NullID(), Method: methodInitialize, Params: json.RawMessage(`{"protocolVersion":1}`),
	})
	if response := readRawResponse(t, stream); response.ID != jsonrpc.NullID() {
		t.Fatalf("the null request ID became %v", response.ID)
	}
}

func TestAnUnimplementedMethodIsNotFound(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	for id, method := range map[int]string{2: methodLogout, 3: "_vendor.example/thing", 4: methodFsReadTextFile} {
		writeRaw(t, stream, rawCall(id, method, `{}`))
		if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
			t.Fatalf("%s was answered %d (%s), want method not found", method, code, code)
		}
	}
}

// A method the other peer serves is not this side's to judge against a
// capability: an agent asked to read a file refuses because agents do not serve
// that method, whatever the client advertised about itself. The judge used to
// read the client's own fs capability and blame it in the refusal.
func TestAMethodTheOtherPeerServesIsNotFound(t *testing.T) {
	for name, agreed := range map[string]Negotiated{
		"the client advertised it for itself": {
			ClientCapabilities: ClientCapabilities{Fs: FileSystemCapabilities{ReadTextFile: true}},
		},
		"the client did not": {},
	} {
		t.Run(name, func(t *testing.T) {
			conn := newTestAgentConn(t)
			conn.negotiation.accept(agreed)
			call, err := conn.requests.accept(t.Context(), inboundFor(jsonrpc.Int64ID(1)))
			if err != nil {
				t.Fatal(err)
			}
			entry := conn.side.register(call, rawCall(1, methodFsReadTextFile, `{"sessionId":"s","path":"/w/f"}`))
			if entry.automatic == nil {
				t.Fatal("an agent dispatched a method only a client serves")
			}
			var failure *Error
			if !errors.As(entry.automatic.err, &failure) || failure.Code != ErrorCodeMethodNotFound || !strings.Contains(failure.Message, "other peer") {
				t.Fatalf("refused with %v, want method-not-found naming the other peer", entry.automatic.err)
			}
		})
	}
}

// A standard notification sent with an id is a request for a method that has no
// request form: method-not-found, as the reference implementation answers it.
func TestAStandardNotificationSentAsARequestIsNotFound(t *testing.T) {
	stream, agentConn := rawAgentConnection(t)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	writeRaw(t, stream, rawCall(2, methodSessionCancel, `{"sessionId":"sess-1"}`))
	if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
		t.Fatalf("session/cancel with an id was answered %d (%s), want method not found", code, code)
	}
}

// The schema permits null params, which mean the same as absent ones: a method
// requiring no property accepts them, and one requiring a property refuses them
// with invalid-params, never by crashing.
func TestNullParamsAreAbsentParams(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt:     endTurn,
		SetMode: func(context.Context, *AgentSession, *SetSessionModeRequest) (*SetSessionModeResponse, error) {
			return &SetSessionModeResponse{}, nil
		},
		LoadSession: func(context.Context, *AgentSession, *LoadSessionRequest) (*LoadSessionResponse, error) {
			return &LoadSessionResponse{}, nil
		},
		ListSessions: func(context.Context, *AgentConn, *ListSessionsRequest) (*ListSessionsResponse, error) {
			return &ListSessionsResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	for id, method := range map[int]string{2: methodSessionSetMode, 3: methodSessionLoad} {
		writeRaw(t, stream, rawCall(id, method, `null`))
		if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidParams {
			t.Fatalf("%s with null params was answered %d (%s), want invalid params", method, code, code)
		}
	}
	writeRaw(t, stream, rawCall(4, methodSessionList, `null`))
	if response := readRawResponse(t, stream); response.Error != nil {
		t.Fatalf("session/list with null params failed: %s", response.Error)
	}
}

// The session a prompt names is decoded once, by the codec that decodes the rest
// of the prompt, so a key matching it only case-insensitively cannot claim the
// turn of another session.
func TestAPromptClaimsTheSessionItsHandlerServes(t *testing.T) {
	release := make(chan struct{})
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	defer close(release)
	initializeRawAgent(t, stream)

	writeRaw(t, stream, rawCall(2, methodSessionPrompt, `{"sessionId":"s","SessionID":"decoy","prompt":[]}`))
	writeRaw(t, stream, rawCall(3, methodSessionPrompt, `{"sessionId":"s","prompt":[]}`))
	response := readRawResponse(t, stream)
	if response.ID != jsonrpc.Int64ID(3) || errorCodeOf(t, response) != ErrorCodeInvalidRequest {
		t.Fatalf("the overlapping prompt for session s was answered %#v, want invalid request", response)
	}
}

// Names without the extension prefix are reserved for later revisions, so no
// fallback may claim one.
func TestAReservedFutureMethodDoesNotReachTheExtensionFallback(t *testing.T) {
	reached := false
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt:     endTurn,
		CallFallback: func(context.Context, *AgentConn, *ExtRequest) (any, error) {
			reached = true
			return struct{}{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	writeRaw(t, stream, rawCall(2, "future/method", `{}`))
	if code := readRawErrorCode(t, stream); code != ErrorCodeMethodNotFound {
		t.Fatalf("the reserved method was answered %d (%s), want method not found", code, code)
	}
	if reached {
		t.Fatal("a protocol-reserved method reached the extension fallback")
	}
}

// Acting on $/cancel_request narrows the legal answer to a valid result or -32800.
func TestACancelledHandlerIsAnsweredWithRequestCancelled(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	writeRaw(t, stream, rawCall(2, methodSessionPrompt, `{"sessionId":"sess-1","prompt":[]}`))
	writeRaw(t, stream, rawNotification(methodCancelRequest, `{"requestId":2}`))
	if code := readRawErrorCode(t, stream); code != ErrorCodeRequestCancelled {
		t.Fatalf("the cancelled prompt was answered %d (%s), want request cancelled", code, code)
	}
}

// A request reusing the id of one still being served is refused on its own; the
// connection and the original request carry on.
func TestReusingAnActiveRequestIDRefusesOnlyTheDuplicate(t *testing.T) {
	release := make(chan struct{})
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			<-release
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.
	initializeRawAgent(t, stream)

	writeRaw(t, stream, rawCall(2, methodSessionPrompt, `{"sessionId":"sess-1","prompt":[]}`))
	writeRaw(t, stream, rawCall(2, methodSessionNew, `{"cwd":"/w","mcpServers":[]}`))
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("the duplicate was answered %d (%s), want invalid request", code, code)
	}
	close(release)
	if response := readRawResponse(t, stream); response.Error != nil {
		t.Fatalf("the original prompt failed: %s", response.Error)
	}
}

func newSessionHandler(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
	return &NewSessionResponse{SessionID: "sess-1"}, nil
}

func endTurn(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
	return &PromptResponse{StopReason: StopReasonEndTurn}, nil
}

func rawAgentConnection(t *testing.T) (Connection, *AgentConn) {
	t.Helper()
	agent, err := NewAgent(&AgentConfig{NewSession: newSessionHandler, Prompt: endTurn})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return connectRawAgent(t, agent)
}
