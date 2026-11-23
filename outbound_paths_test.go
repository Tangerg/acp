package acp_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Tangerg/acp"
)

func TestSessionSetupRejectsRelativeMCPExecutables(t *testing.T) {
	var served atomic.Int32
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			served.Add(1)
			return &acp.NewSessionResponse{SessionID: "session"}, nil
		},
		LoadSession: func(context.Context, *acp.AgentSession, *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
			served.Add(1)
			return &acp.LoadSessionResponse{}, nil
		},
		ResumeSession: func(context.Context, *acp.AgentSession, *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
			served.Add(1)
			return &acp.ResumeSessionResponse{}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := connectPeers(t, testClient(t), agent)
	for _, command := range []string{"./server", "/opt/bin/server"} {
		servers := []acp.McpServer{&acp.McpServerStdio{Name: "server", Command: command}}
		operations := []struct {
			name string
			call func() error
		}{
			{"new", func() error {
				_, _, err := conn.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/workspace", McpServers: servers})
				return err
			}},
			{"load", func() error {
				_, _, err := conn.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: "session", Cwd: "/workspace", McpServers: servers})
				return err
			}},
			{"resume", func() error {
				_, _, err := conn.ResumeSession(t.Context(), &acp.ResumeSessionRequest{SessionID: "session", Cwd: "/workspace", McpServers: servers})
				return err
			}},
		}
		for _, operation := range operations {
			t.Run(operation.name+command, func(t *testing.T) {
				before := served.Load()
				err := operation.call()
				if command == "./server" {
					if err == nil || !strings.Contains(err.Error(), "mcpServers[0].command") {
						t.Errorf("relative command: %v", err)
					}
					if served.Load() != before {
						t.Error("invalid path crossed the transport")
					}
				} else if err != nil || served.Load() != before+1 {
					t.Errorf("valid command = %v; calls = %d", err, served.Load()-before)
				}
			})
		}
	}
}

func TestSessionListChecksRequestAndResponsePaths(t *testing.T) {
	for _, invalid := range []string{"filter", "cwd", "additionalDirectories", "none"} {
		t.Run(invalid, func(t *testing.T) {
			var served atomic.Bool
			agent, err := acp.NewAgent(&acp.AgentConfig{
				NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
					return &acp.NewSessionResponse{SessionID: "session"}, nil
				},
				Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
					return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				},
				ListSessions: func(context.Context, *acp.AgentConn, *acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
					served.Store(true)
					info := acp.SessionInfo{SessionID: "session", Cwd: "/workspace", AdditionalDirectories: []string{"/shared"}}
					if invalid == "cwd" {
						info.Cwd = "relative"
					}
					if invalid == "additionalDirectories" {
						info.AdditionalDirectories[0] = "relative"
					}
					return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{info}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			conn := connectPeers(t, testClient(t), agent)
			cwd := "/workspace"
			if invalid == "filter" {
				cwd = "relative"
			}
			_, err = conn.ListSessions(t.Context(), &acp.ListSessionsRequest{Cwd: acp.OptValue(cwd)})
			if invalid == "none" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if invalid == "filter" {
				// The caller's own relative path is refused locally, before the wire.
				if served.Load() {
					t.Error("invalid filter crossed the transport")
				}
				if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
					t.Errorf("invalid filter: %v, want a local absolute-path refusal", err)
				}
				return
			}
			var failure *acp.Error
			if !errors.As(err, &failure) || failure.Code != acp.ErrorCodeInternalError {
				t.Errorf("invalid %s: %v, want internal error", invalid, err)
			}
		})
	}
}

func TestToolCallPathsAreCheckedBeforeUpdatesAndPermissionRequests(t *testing.T) {
	for _, operation := range []string{"tool call", "tool call update", "permission"} {
		for _, field := range []string{"diff", "location"} {
			for _, path := range []string{"relative", "/workspace/file"} {
				t.Run(operation+"/"+field+"/"+path, func(t *testing.T) {
					var delivered atomic.Int32
					client, err := acp.NewClient(&acp.ClientConfig{
						SessionUpdate: func(context.Context, *acp.ClientConn, *acp.SessionNotification) { delivered.Add(1) },
						RequestPermission: func(context.Context, *acp.ClientConn, *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
							delivered.Add(1)
							return &acp.RequestPermissionResponse{Outcome: &acp.RequestPermissionOutcomeCancelled{}}, nil
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					result := make(chan error, 1)
					agent := testAgent(t, func(ctx context.Context, session *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
						var content []acp.ToolCallContent
						var locations []acp.ToolCallLocation
						if field == "diff" {
							content = []acp.ToolCallContent{&acp.Diff{Path: path, NewText: "changed"}}
						}
						if field == "location" {
							locations = []acp.ToolCallLocation{{Path: path}}
						}
						var callErr error
						update := acp.ToolCallUpdate{ToolCallID: "tool", Content: acp.OptValue(content), Locations: acp.OptValue(locations)}
						switch operation {
						case "permission":
							_, callErr = session.RequestPermission(ctx, &acp.RequestPermissionParams{ToolCall: update})
						case "tool call update":
							callErr = session.Update(ctx, &acp.SessionUpdateParams{Update: &update})
						case "tool call":
							callErr = session.Update(ctx, &acp.SessionUpdateParams{Update: &acp.ToolCall{ToolCallID: "tool", Title: "Edit", Content: content, Locations: locations}})
						}
						result <- callErr
						return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
					})
					session := connectAndOpen(t, client, agent)
					if _, err := session.Prompt(t.Context(), nil); err != nil {
						t.Fatal(err)
					}
					callErr := <-result
					if path == "relative" {
						if callErr == nil || delivered.Load() != 0 {
							t.Errorf("invalid path: %v, deliveries=%d", callErr, delivered.Load())
						}
					} else if callErr != nil || delivered.Load() != 1 {
						t.Errorf("valid path: %v, deliveries=%d", callErr, delivered.Load())
					}
				})
			}
		}
	}
}
