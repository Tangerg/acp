package acp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestClientCallbacksIdentifyTheirSourceWithOverlappingPeerIDs(t *testing.T) {
	type source struct {
		name string
		conn *ClientConn
	}
	seen := make(chan source, 1)
	record := func(name string, conn *ClientConn) { seen <- source{name: name, conn: conn} }
	client, err := NewClient(&ClientConfig{
		SessionUpdate: func(_ context.Context, conn *ClientConn, _ *SessionNotification) { record("update", conn) },
		RequestPermission: func(_ context.Context, conn *ClientConn, _ *RequestPermissionRequest) (*RequestPermissionResponse, error) {
			record("permission", conn)
			return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
		},
		ReadTextFile: func(_ context.Context, conn *ClientConn, _ *ReadTextFileRequest) (*ReadTextFileResponse, error) {
			record("read", conn)
			return &ReadTextFileResponse{}, nil
		},
		WriteTextFile: func(_ context.Context, conn *ClientConn, _ *WriteTextFileRequest) (*WriteTextFileResponse, error) {
			record("write", conn)
			return &WriteTextFileResponse{}, nil
		},
		Terminal: &TerminalHandlers{
			Create: func(_ context.Context, conn *ClientConn, _ *CreateTerminalRequest) (*CreateTerminalResponse, error) {
				record("create", conn)
				return &CreateTerminalResponse{TerminalID: "same-terminal"}, nil
			},
			Output: func(_ context.Context, conn *ClientConn, _ *TerminalOutputRequest) (*TerminalOutputResponse, error) {
				record("output", conn)
				return &TerminalOutputResponse{}, nil
			},
			WaitForExit: func(_ context.Context, conn *ClientConn, _ *WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error) {
				record("wait", conn)
				return &WaitForTerminalExitResponse{}, nil
			},
			Kill: func(_ context.Context, conn *ClientConn, _ *KillTerminalRequest) (*KillTerminalResponse, error) {
				record("kill", conn)
				return &KillTerminalResponse{}, nil
			},
			Release: func(_ context.Context, conn *ClientConn, _ *ReleaseTerminalRequest) (*ReleaseTerminalResponse, error) {
				record("release", conn)
				return &ReleaseTerminalResponse{}, nil
			},
		},
		Elicitation: &ElicitationHandlers{
			Form: func(_ context.Context, conn *ClientConn, _ *CreateElicitationRequest, _ *ElicitationFormMode) (*CreateElicitationResponse, error) {
				record("form", conn)
				return &CreateElicitationResponse{Value: &CreateElicitationResponseCancel{}}, nil
			},
			URL: func(_ context.Context, conn *ClientConn, _ *CreateElicitationRequest, _ *ElicitationURLMode) (*CreateElicitationResponse, error) {
				record("url", conn)
				return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
			},
			Complete: func(_ context.Context, conn *ClientConn, _ *CompleteElicitationNotification) {
				record("complete", conn)
			},
		},
		CallFallback: func(_ context.Context, conn *ClientConn, _ *ExtRequest) (any, error) {
			record("extension call", conn)
			return json.RawMessage(`{}`), nil
		},
		NotifyFallback: func(_ context.Context, conn *ClientConn, _ *ExtNotification) { record("extension notification", conn) },
	})
	if err != nil {
		t.Fatal(err)
	}
	firstStream, first := connectRawClient(t, client)
	secondStream, second := connectRawClient(t, client)
	peers := []struct {
		stream Connection
		conn   *ClientConn
	}{{firstStream, first}, {secondStream, second}}
	requests := []struct {
		name, method, params string
		notification         bool
	}{
		{"update", methodSessionUpdate, `{"sessionId":"same-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}`, true},
		{"permission", methodSessionRequestPermission, `{"sessionId":"same-session","toolCall":{"toolCallId":"same-tool"},"options":[]}`, false},
		{"read", methodFsReadTextFile, `{"sessionId":"same-session","path":"/workspace/file"}`, false},
		{"write", methodFsWriteTextFile, `{"sessionId":"same-session","path":"/workspace/file","content":"changed"}`, false},
		{"create", methodTerminalCreate, `{"sessionId":"same-session","command":"/bin/true"}`, false},
		{"output", methodTerminalOutput, `{"sessionId":"same-session","terminalId":"same-terminal"}`, false},
		{"wait", methodTerminalWaitForExit, `{"sessionId":"same-session","terminalId":"same-terminal"}`, false},
		{"kill", methodTerminalKill, `{"sessionId":"same-session","terminalId":"same-terminal"}`, false},
		{"release", methodTerminalRelease, `{"sessionId":"same-session","terminalId":"same-terminal"}`, false},
		{"form", methodElicitationCreate, `{"message":"form","mode":"form","requestedSchema":{"type":"object","properties":{}},"requestId":7}`, false},
		{"url", methodElicitationCreate, `{"message":"url","mode":"url","url":"https://example.invalid/auth","elicitationId":"same-elicitation","sessionId":"same-session"}`, false},
		{"complete", methodElicitationComplete, `{"elicitationId":"same-elicitation"}`, true},
		{"extension call", "_test/source", `{}`, false},
		{"extension notification", "_test/source", `{}`, true},
	}
	ctx, cancel := context.WithTimeout(t.Context(), deadlockGuard)
	defer cancel()
	for _, request := range requests {
		for _, peer := range peers {
			message := rawCall(7, request.method, request.params)
			if request.notification {
				message = rawNotification(request.method, request.params)
			}
			if err := peer.stream.Write(ctx, message); err != nil {
				t.Fatal(err)
			}
			if !request.notification {
				message, err := peer.stream.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				response, ok := message.(*jsonrpc.Response)
				if !ok || response.Error != nil {
					t.Fatalf("%s returned %#v", request.name, message)
				}
			}
			select {
			case got := <-seen:
				if got.name != request.name || got.conn != peer.conn {
					t.Errorf("%s source = %s %p, want %p", request.name, got.name, got.conn, peer.conn)
				}
			case <-ctx.Done():
				t.Fatalf("%s callback was not delivered: %v", request.name, ctx.Err())
			}
		}
	}
}

func TestAgentExtensionsIdentifyTheirSourceWithOverlappingRequestIDs(t *testing.T) {
	seen := make(chan *AgentConn, 1)
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "same-session"}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
		CallFallback: func(_ context.Context, conn *AgentConn, _ *ExtRequest) (any, error) {
			seen <- conn
			return json.RawMessage(`{}`), nil
		},
		NotifyFallback: func(_ context.Context, conn *AgentConn, _ *ExtNotification) { seen <- conn },
	})
	if err != nil {
		t.Fatal(err)
	}
	firstStream, first := connectRawAgent(t, agent)
	secondStream, second := connectRawAgent(t, agent)
	peers := []struct {
		stream Connection
		conn   *AgentConn
	}{{firstStream, first}, {secondStream, second}}
	ctx, cancel := context.WithTimeout(t.Context(), deadlockGuard)
	defer cancel()
	for _, peer := range peers {
		handshakeRaw(ctx, t, peer.stream)
	}
	for _, notification := range []bool{false, true} {
		for _, peer := range peers {
			message := rawCall(7, "_test/source", `{}`)
			if notification {
				message = rawNotification("_test/source", `{}`)
			}
			if err := peer.stream.Write(ctx, message); err != nil {
				t.Fatal(err)
			}
			if !notification {
				message, err := peer.stream.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				response, ok := message.(*jsonrpc.Response)
				if !ok || response.Error != nil {
					t.Fatalf("extension returned %#v", message)
				}
			}
			select {
			case got := <-seen:
				if got != peer.conn {
					t.Errorf("extension source = %p, want %p", got, peer.conn)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
}
