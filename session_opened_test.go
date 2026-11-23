package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/acp"
	"github.com/Tangerg/acp/jsonrpc"
)

// An update may only name a session the client has been given, and session/new
// gives it the identifier in its answer. So an agent's announcement for a
// session it has just opened, such as its slash commands, comes after that
// answer on the wire. Zed shows no commands for a new thread until it hears
// them, so announcing anywhere later leaves the first message without any.
func TestAnOpenedSessionIsAnnouncedAfterTheAnswerThatOpensIt(t *testing.T) {
	tests := map[string]struct {
		method, params string
		session        acp.SessionID
	}{
		"new":    {"session/new", `{"cwd":"/w","mcpServers":[]}`, "created"},
		"load":   {"session/load", `{"sessionId":"stored","cwd":"/w","mcpServers":[]}`, "stored"},
		"resume": {"session/resume", `{"sessionId":"stored","cwd":"/w"}`, "stored"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stream, ctx := rawClientFor(t, openingAgent(t, nil, nil))
			initializeRaw(ctx, t, stream)

			writeRaw(ctx, t, stream, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":%q,"params":%s}`,
				test.method, test.params))
			answer, ok := readRaw(ctx, t, stream).(*jsonrpc.Response)
			if !ok || answer.Error != nil {
				t.Fatalf("%s was not answered first with a result: %+v", test.method, answer)
			}
			update, ok := readRaw(ctx, t, stream).(*jsonrpc.Request)
			if !ok || update.Method != "session/update" {
				t.Fatalf("the answer was followed by %+v, want the announcement", update)
			}
			var notification acp.SessionNotification
			if err := json.Unmarshal(update.Params, &notification); err != nil {
				t.Fatal(err)
			}
			if notification.SessionID != test.session {
				t.Errorf("the announcement names %q, want %q", notification.SessionID, test.session)
			}
			if _, ok := notification.Update.(*acp.AvailableCommandsUpdate); !ok {
				t.Errorf("the announcement is a %T", notification.Update)
			}
		})
	}
}

// A session the client was refused is not one it holds, so nothing announces it.
func TestARefusedOpenIsNotAnnounced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var announced atomic.Int32
		refuse := func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return nil, errors.New("no sessions today")
		}
		stream, ctx := rawClientFor(t, openingAgent(t, refuse, func(context.Context, *acp.AgentSession) {
			announced.Add(1)
		}))
		initializeRaw(ctx, t, stream)

		answer := roundTrip(ctx, t, stream, 2, "session/new", `{"cwd":"/w","mcpServers":[]}`)
		if answer.Error == nil {
			t.Fatal("the refused session/new was answered with a result")
		}
		synctest.Wait()
		if n := announced.Load(); n != 0 {
			t.Fatalf("a refused session was announced %d times", n)
		}
	})
}

// The request that opened a session is answered, so an announcement that takes
// its time must not keep counting against InflightRequests: breaching that bound
// ends the connection.
func TestASlowAnnouncementDoesNotHoldItsRequestsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		agent := openingAgentWith(t, acp.Limits{InflightRequests: 1}, nil, func(context.Context, *acp.AgentSession) {
			<-release
		})
		stream, ctx := rawClientFor(t, agent)
		initializeRaw(ctx, t, stream)
		// A request's place is returned just after its answer is written, so the
		// next one waits until it has been.
		synctest.Wait()

		for id := int64(2); id <= 3; id++ {
			answer := roundTrip(ctx, t, stream, id, "session/new", `{"cwd":"/w","mcpServers":[]}`)
			if answer.Error != nil {
				t.Fatalf("session/new %d was refused: %v", id, answer.Error)
			}
			// Every goroutine has settled, so the first announcement is blocked
			// before the second request arrives.
			synctest.Wait()
		}
		close(release)
	})
}

// openingAgent opens "created" from session/new and serves load and resume for
// any stored session, announcing each one it opens. newSession and opened
// replace those defaults when set.
func openingAgent(
	t *testing.T,
	newSession func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error),
	opened func(context.Context, *acp.AgentSession),
) *acp.Agent {
	t.Helper()
	return openingAgentWith(t, acp.Limits{}, newSession, opened)
}

func openingAgentWith(
	t *testing.T,
	limits acp.Limits,
	newSession func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error),
	opened func(context.Context, *acp.AgentSession),
) *acp.Agent {
	t.Helper()
	if newSession == nil {
		newSession = func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "created"}, nil
		}
	}
	if opened == nil {
		opened = func(ctx context.Context, session *acp.AgentSession) {
			commands := &acp.AvailableCommandsUpdate{AvailableCommands: []acp.AvailableCommand{
				{Name: "plan", Description: "Plan before editing"},
			}}
			if err := session.Update(ctx, &acp.SessionUpdateParams{Update: commands}); err != nil {
				t.Errorf("announcing %s: %v", session.ID(), err)
			}
		}
	}
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: newSession,
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		// A handler editing its request cannot redirect the announcement: the
		// session is the one the client named.
		LoadSession: func(_ context.Context, _ *acp.AgentSession, request *acp.LoadSessionRequest) (*acp.LoadSessionResponse, error) {
			request.SessionID = "handler-edited-session"
			return &acp.LoadSessionResponse{}, nil
		},
		ResumeSession: func(_ context.Context, _ *acp.AgentSession, request *acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
			request.SessionID = "handler-edited-session"
			return &acp.ResumeSessionResponse{}, nil
		},
		SessionOpened: opened,
		Limits:        limits,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}
