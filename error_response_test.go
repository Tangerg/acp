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
		{name: "messages", pair: func() (acp.Transport, acp.Transport) { return acp.NewInMemoryTransports() }},
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
						CallFallback: func(context.Context, *acp.AgentConn, *acp.ExtRequest) (any, error) {
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

// answerNothing is what a handler that breaks its contract returns.
func answerNothing[Response any]() (*Response, error) {
	return nil, nil //nolint:nilnil // the contract violation under test.
}

// A handler that returns neither a response nor an error has broken its
// contract, and the peer is told so with -32603 rather than handed a null result
// it cannot decode. Every request row is built by one constructor that keeps
// this rule, so the methods the connection serves in its own way are held to it
// as well as the plain ones: the elicitation handlers used to slip past it and
// put "result":null on the wire.
func TestAHandlerThatAnswersNothingIsAnInternalError(t *testing.T) {
	wantInternalError := func(t *testing.T, err error) {
		t.Helper()
		var failure *acp.Error
		if !errors.As(err, &failure) || failure.Code != acp.ErrorCodeInternalError {
			t.Fatalf("the peer saw %v, want an internal error", err)
		}
	}

	t.Run("an agent's handlers", func(t *testing.T) {
		for name, call := range map[string]func(*acp.ClientConn) error{
			"session/new": func(conn *acp.ClientConn) error {
				_, _, err := conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/nil"})
				return err
			},
			"session/set_mode": func(conn *acp.ClientConn) error {
				session, _, err := conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/w"})
				if err != nil {
					return err
				}
				_, err = session.SetMode(t.Context(), &acp.SetSessionModeParams{ModeID: "fast"})
				return err
			},
			"session/prompt": func(conn *acp.ClientConn) error {
				session, _, err := conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/w"})
				if err != nil {
					return err
				}
				_, err = session.Prompt(t.Context(), &acp.PromptParams{})
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				agent, err := acp.NewAgent(&acp.AgentConfig{
					NewSession: func(_ context.Context, _ *acp.AgentConn, request *acp.NewSessionRequest) (
						*acp.NewSessionResponse, error,
					) {
						if request.Cwd == "/nil" {
							return answerNothing[acp.NewSessionResponse]()
						}
						return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
					},
					Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
						return answerNothing[acp.PromptResponse]()
					},
					SetMode: func(context.Context, *acp.AgentSession, *acp.SetSessionModeRequest) (
						*acp.SetSessionModeResponse, error,
					) {
						return answerNothing[acp.SetSessionModeResponse]()
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				wantInternalError(t, call(connectPeers(t, testClient(t), agent)))
			})
		}
	})

	t.Run("a client's handlers", func(t *testing.T) {
		for name, test := range map[string]struct {
			configure func(*acp.ClientConfig)
			call      func(context.Context, *acp.AgentSession) error
		}{
			"session/request_permission": {
				configure: func(config *acp.ClientConfig) {
					config.RequestPermission = func(context.Context, *acp.ClientConn, *acp.RequestPermissionRequest) (
						*acp.RequestPermissionResponse, error,
					) {
						return answerNothing[acp.RequestPermissionResponse]()
					}
				},
				call: func(ctx context.Context, session *acp.AgentSession) error {
					_, err := session.RequestPermission(ctx, &acp.RequestPermissionParams{
						ToolCall: acp.ToolCallUpdate{ToolCallID: "write-1"},
						Options: []acp.PermissionOption{
							{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
						},
					})
					return err
				},
			},
			"fs/read_text_file": {
				configure: func(config *acp.ClientConfig) {
					config.ReadTextFile = func(context.Context, *acp.ClientConn, *acp.ReadTextFileRequest) (
						*acp.ReadTextFileResponse, error,
					) {
						return answerNothing[acp.ReadTextFileResponse]()
					}
				},
				call: func(ctx context.Context, session *acp.AgentSession) error {
					_, err := session.ReadTextFile(ctx, &acp.ReadTextFileParams{Path: "/w/notes"})
					return err
				},
			},
			"a form elicitation": {
				configure: func(config *acp.ClientConfig) {
					config.Elicitation = &acp.ElicitationHandlers{Form: func(
						context.Context, *acp.ClientConn, *acp.CreateElicitationRequest, *acp.ElicitationFormMode,
					) (*acp.CreateElicitationResponse, error) {
						return answerNothing[acp.CreateElicitationResponse]()
					}}
				},
				call: func(ctx context.Context, session *acp.AgentSession) error {
					_, err := session.CreateElicitation(ctx, &acp.CreateElicitationParams{
						Message: "which branch?", Mode: &acp.ElicitationFormMode{},
					})
					return err
				},
			},
			"a URL elicitation": {
				configure: func(config *acp.ClientConfig) {
					config.Elicitation = &acp.ElicitationHandlers{URL: func(
						context.Context, *acp.ClientConn, *acp.CreateElicitationRequest, *acp.ElicitationURLMode,
					) (*acp.CreateElicitationResponse, error) {
						return answerNothing[acp.CreateElicitationResponse]()
					}}
				},
				call: func(ctx context.Context, session *acp.AgentSession) error {
					_, err := session.CreateElicitation(ctx, &acp.CreateElicitationParams{
						Message: "sign in", Mode: &acp.ElicitationURLMode{ElicitationID: "e-1", URL: "https://example.com"},
					})
					return err
				},
			},
		} {
			t.Run(name, func(t *testing.T) {
				config := &acp.ClientConfig{
					SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
					RequestPermission: denyingPermission,
				}
				test.configure(config)
				client, err := acp.NewClient(config)
				if err != nil {
					t.Fatal(err)
				}
				observed := make(chan error, 1)
				session := connectAndOpen(t, client, testAgent(t, func(
					ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest,
				) (*acp.PromptResponse, error) {
					observed <- test.call(ctx, session)
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				}))
				if _, err := session.Prompt(t.Context(), &acp.PromptParams{}); err != nil {
					t.Fatalf("Prompt: %v", err)
				}
				wantInternalError(t, <-observed)
			})
		}
	})
}
