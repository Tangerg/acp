package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/Tangerg/acp"
)

func TestMalformedHandlerErrorDoesNotCloseTheConnection(t *testing.T) {
	for _, test := range []struct {
		name string
		pair func() (acp.Transport, acp.Transport)
	}{
		{name: "messages", pair: acp.NewInMemoryTransports},
		{name: "bytes", pair: crossedIOTransports},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, invalid := range []struct {
				name string
				data json.RawMessage
			}{
				{name: "incomplete JSON", data: json.RawMessage(`{`)},
				{name: "empty value", data: json.RawMessage{}},
				{name: "invalid UTF-8", data: json.RawMessage{'"', 0xff, '"'}},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					agent, err := acp.NewAgent(&acp.AgentConfig{
						NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
							return nil, &acp.Error{
								Code: acp.ErrorCodeInvalidParams, Message: "malformed local error",
								Data: acp.OptValue(invalid.data),
							}
						},
						Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
							return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
						},
						Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
						CallFallback: func(context.Context, *acp.AgentConn, *acp.ExtRequest) (json.RawMessage, error) {
							return json.RawMessage(`{}`), nil
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					clientSide, agentSide := test.pair()
					agentConn, err := agent.Connect(t.Context(), agentSide)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = agentConn.Close() })
					conn, err := testClient(t).Connect(t.Context(), clientSide)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = conn.Close() })
					_, _, err = conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/w"})
					var failure *acp.Error
					if !errors.As(err, &failure) || failure.Code != acp.ErrorCodeInternalError {
						t.Fatalf("malformed local error returned %v, want a request-scoped internal error", err)
					}
					var result json.RawMessage
					if err := conn.Call(t.Context(), "_probe", nil, &result); err != nil {
						t.Fatalf("an unrelated request failed after the error: %v", err)
					}
					if string(result) != `{}` {
						t.Errorf("unrelated response = %s", result)
					}
				})
			}
		})
	}
}

func crossedIOTransports() (acp.Transport, acp.Transport) {
	clientRead, agentWrite := io.Pipe()
	agentRead, clientWrite := io.Pipe()
	return acp.NewIOTransport(&acp.IOConfig{
			Reader: clientRead, Writer: clientWrite,
			Close: func() error { return errors.Join(clientRead.Close(), clientWrite.Close()) },
		}), acp.NewIOTransport(&acp.IOConfig{
			Reader: agentRead, Writer: agentWrite,
			Close: func() error { return errors.Join(agentRead.Close(), agentWrite.Close()) },
		})
}

func TestProtocolSentinelsRetainTheirCodesAsHandlerErrors(t *testing.T) {
	for _, sentinel := range []error{acp.ErrAuthRequired, acp.ErrRequestCancelled} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/wrapped=%t", sentinel, wrapped), func(t *testing.T) {
				failure := sentinel
				if wrapped {
					failure = fmt.Errorf("operation failed: %w", failure)
				}
				agent, err := acp.NewAgent(&acp.AgentConfig{
					NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
						return nil, failure
					},
					Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
						return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
					},
					Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
				})
				if err != nil {
					t.Fatal(err)
				}
				conn := connectPeers(t, testClient(t), agent)
				_, _, err = conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/workspace"})
				if !errors.Is(err, sentinel) {
					t.Errorf("handler returned %v; peer received %v", failure, err)
				}
			})
		}
	}
}
