package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestBooleanConfigurationIsGatedAtEveryAgentOutput(t *testing.T) {
	capabilities := []struct {
		name      string
		session   Opt[ClientSessionCapabilities]
		permitted bool
	}{
		{name: "absent"},
		{name: "null session", session: OptNull[ClientSessionCapabilities]()},
		{name: "null options", session: OptValue(ClientSessionCapabilities{ConfigOptions: OptNull[SessionConfigOptionsCapabilities]()})},
		{name: "null boolean", session: OptValue(ClientSessionCapabilities{ConfigOptions: OptValue(SessionConfigOptionsCapabilities{Boolean: OptNull[BooleanConfigOptionCapabilities]()})})},
		{name: "advertised", session: OptValue(ClientSessionCapabilities{ConfigOptions: OptValue(SessionConfigOptionsCapabilities{Boolean: OptValue(BooleanConfigOptionCapabilities{})})}), permitted: true},
	}
	for _, capability := range capabilities {
		t.Run(capability.name, func(t *testing.T) {
			options := []SessionConfigOption{{ID: "enabled", Name: "Enabled", Value: &SessionConfigBoolean{CurrentValue: true}}}
			agent, err := NewAgent(&AgentConfig{
				NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
					return &NewSessionResponse{SessionID: "session", ConfigOptions: OptValue(options)}, nil
				},
				LoadSession: func(context.Context, *AgentSession, *LoadSessionRequest) (*LoadSessionResponse, error) {
					return &LoadSessionResponse{ConfigOptions: OptValue(options)}, nil
				},
				ResumeSession: func(context.Context, *AgentSession, *ResumeSessionRequest) (*ResumeSessionResponse, error) {
					return &ResumeSessionResponse{ConfigOptions: OptValue(options)}, nil
				},
				SetConfigOption: func(context.Context, *AgentSession, *SetSessionConfigOptionRequest) (*SetSessionConfigOptionResponse, error) {
					return &SetSessionConfigOptionResponse{ConfigOptions: options}, nil
				},
				Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
					return &PromptResponse{StopReason: StopReasonEndTurn}, nil
				},
				Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
			})
			if err != nil {
				t.Fatal(err)
			}
			stream, conn := connectRawAgent(t, agent)
			t.Cleanup(func() { _ = conn.Close() })
			initialize, err := json.Marshal(InitializeRequest{ProtocolVersion: CurrentProtocolVersion, ClientCapabilities: ClientCapabilities{Session: capability.session}})
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Write(t.Context(), rawCall(1, methodInitialize, string(initialize))); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Read(t.Context()); err != nil {
				t.Fatal(err)
			}
			requests := []struct{ method, params string }{
				{methodSessionNew, `{"cwd":"/workspace","mcpServers":[]}`},
				{methodSessionLoad, `{"sessionId":"session","cwd":"/workspace","mcpServers":[]}`},
				{methodSessionResume, `{"sessionId":"session","cwd":"/workspace","mcpServers":[]}`},
				{methodSessionSetConfigOption, `{"sessionId":"session","configId":"enabled","value":"selected"}`},
			}
			for i, request := range requests {
				t.Run(request.method, func(t *testing.T) {
					if err := stream.Write(t.Context(), rawCall(i+2, request.method, request.params)); err != nil {
						t.Fatal(err)
					}
					message, err := stream.Read(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					response, ok := message.(*jsonrpc.Response)
					if !ok {
						t.Fatalf("read %T, want a response", message)
					}
					if capability.permitted {
						if response.Error != nil {
							t.Errorf("advertised boolean response: %v", response.Error)
						}
					} else if response.Error == nil || response.Error.Code != int64(ErrorCodeInternalError) {
						t.Errorf("unadvertised boolean response: %s / %v", response.Result, response.Error)
					}
				})
			}
			t.Run("session/update", func(t *testing.T) {
				result := make(chan error, 1)
				go func() {
					result <- conn.session("session").Update(t.Context(), &SessionUpdateParams{Update: &ConfigOptionUpdate{ConfigOptions: options}})
				}()
				if !capability.permitted {
					var failure *Error
					if err := <-result; !errors.As(err, &failure) || failure.Code != ErrorCodeInvalidParams {
						t.Errorf("unadvertised update: %v", err)
					}
					return
				}
				message, err := stream.Read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if update, ok := message.(*jsonrpc.Request); !ok || update.Method != methodSessionUpdate {
					t.Fatalf("read %T, want session/update", message)
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestSessionUpdatesRequireTheirSpecificClientCapabilities(t *testing.T) {
	capabilities := []struct {
		name                string
		session             Opt[ClientSessionCapabilities]
		notices, compaction bool
	}{
		{name: "absent"},
		{name: "null session", session: OptNull[ClientSessionCapabilities]()},
		{name: "null capabilities", session: OptValue(ClientSessionCapabilities{Notices: OptNull[NoticeCapabilities](), Compaction: OptNull[CompactionCapabilities]()})},
		{name: "notices", session: OptValue(ClientSessionCapabilities{Notices: OptValue(NoticeCapabilities{})}), notices: true},
		{name: "compaction", session: OptValue(ClientSessionCapabilities{Compaction: OptValue(CompactionCapabilities{})}), compaction: true},
	}
	for _, capability := range capabilities {
		t.Run(capability.name, func(t *testing.T) {
			stream, conn := rawAgentConnection(t)
			t.Cleanup(func() { _ = conn.Close() })
			initialize, err := json.Marshal(InitializeRequest{ProtocolVersion: CurrentProtocolVersion, ClientCapabilities: ClientCapabilities{Session: capability.session}})
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Write(t.Context(), rawCall(1, methodInitialize, string(initialize))); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Read(t.Context()); err != nil {
				t.Fatal(err)
			}
			updates := []struct {
				name      string
				update    SessionUpdate
				permitted bool
			}{
				{"notice", &Notice{Severity: NoticeSeverityInfo, Title: "Information"}, capability.notices},
				{"compaction", &CompactionUpdate{CompactionID: "compaction", Status: CompactionStatusInProgress}, capability.compaction},
				{"summary", &CompactionSummaryChunk{CompactionID: "compaction", Content: &TextContent{Text: "summary"}}, capability.compaction},
			}
			for _, update := range updates {
				t.Run(update.name, func(t *testing.T) {
					type received struct {
						message jsonrpc.Message
						err     error
					}
					read := make(chan received, 1)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					go func() { message, err := stream.Read(ctx); read <- received{message: message, err: err} }()
					err := conn.session("session").Update(t.Context(), &SessionUpdateParams{Update: update.update})
					cancel()
					incoming := <-read
					if !update.permitted {
						var failure *Error
						if !errors.As(err, &failure) || failure.Code != ErrorCodeInvalidParams {
							t.Errorf("unadvertised update returned %v", err)
						}
						if incoming.message != nil {
							t.Error("unadvertised update crossed the transport")
						}
						return
					}
					if err != nil || incoming.err != nil {
						t.Fatalf("advertised update = %v; read = %v", err, incoming.err)
					}
					if notification, ok := incoming.message.(*jsonrpc.Request); !ok || notification.Method != methodSessionUpdate {
						t.Fatalf("read %T, want session/update", incoming.message)
					}
				})
			}
		})
	}
}
