package acp_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// The session lifecycle beyond a turn: logout, list, delete, resume, close, and
// the configuration options an agent offers.
//
// Every one is optional in the schema and gated on a capability, so each is
// tested twice: that it works when the agent said it was there, and that it is
// refused when the agent did not.

// An agent advertises exactly what its handlers can serve, without being told to.
func TestTheLifecycleHandlersAdvertiseThemselves(t *testing.T) {
	session := connectAndOpen(t, testClient(t), lifecycleAgent(t, nil))
	agent := session.Conn().Peer().AgentCapabilities

	if !hasValue(agent.Auth.Logout) {
		t.Error("a Logout handler did not advertise agentCapabilities.auth.logout")
	}
	for name, advertised := range map[string]bool{
		"list":   hasValue(agent.SessionCapabilities.List),
		"delete": hasValue(agent.SessionCapabilities.Delete),
		"resume": hasValue(agent.SessionCapabilities.Resume),
		"close":  hasValue(agent.SessionCapabilities.Close),
	} {
		if !advertised {
			t.Errorf("a handler did not advertise agentCapabilities.sessionCapabilities.%s", name)
		}
	}
}

// Each operation reaches its handler, and carries what the schema says it carries.
func TestTheLifecycleOperationsRoundTrip(t *testing.T) {
	served := make(chan string, 8)
	conn := connectAndOpen(t, testClient(t), lifecycleAgent(t, served)).Conn()
	ctx := context.Background()

	if _, err := conn.Logout(ctx, nil); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// The cursor is the agent's own opaque token, so it has to survive the round
	// trip in both directions untouched.
	listed, err := conn.ListSessions(ctx, &acp.ListSessionsRequest{Cwd: acp.OptValue("/w")})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if next, more := listed.NextCursor.Get(); !more || next != "page-2" {
		t.Fatalf("the agent's cursor came back as %q, %t", next, more)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != "stored-1" {
		t.Fatalf("the listing came back as %+v", listed.Sessions)
	}
	if _, paged := conn.ListSessions(ctx, &acp.ListSessionsRequest{Cursor: acp.OptValue("page-2")}); paged != nil {
		t.Fatalf("the second page: %v", paged)
	}

	if _, deleted := conn.DeleteSession(ctx, &acp.DeleteSessionRequest{SessionID: "stored-1"}); deleted != nil {
		t.Fatalf("DeleteSession: %v", deleted)
	}

	resumed, answer, err := conn.ResumeSession(ctx, &acp.ResumeSessionRequest{
		SessionID: "stored-1",
		Cwd:       "/w",
	})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resumed.ID() != "stored-1" {
		t.Fatalf("the resumed handle names %q", resumed.ID())
	}
	if answer == nil {
		t.Fatal("resume returned no answer")
	}

	if _, err := resumed.SetConfigOption(ctx, &acp.SetConfigOptionParams{
		ConfigID: "verbosity",
		Value:    &acp.SetSessionConfigOptionRequestValueID{Value: "high"},
	}); err != nil {
		t.Fatalf("SetConfigOption: %v", err)
	}

	if _, err := resumed.Close(ctx, nil); err != nil {
		t.Fatalf("Close: %v", err)
	}

	close(served)
	var reached []string
	for method := range served {
		reached = append(reached, method)
	}
	want := []string{"logout", "list", "list", "delete", "resume", "set_config_option", "close"}
	if strings.Join(reached, ",") != strings.Join(want, ",") {
		t.Fatalf("the agent served %v, want %v", reached, want)
	}
}

// An agent that advertised none of them refuses each one locally, naming the
// capability, rather than spending a round trip to be told the same thing.
func TestTheLifecycleOperationsAreRefusedWhenUnadvertised(t *testing.T) {
	conn := connectAndOpen(t, testClient(t), testAgent(t, nil)).Conn()
	ctx := context.Background()
	session, _, err := conn.NewSession(ctx, &acp.NewSessionRequest{Cwd: "/w", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	refusals := map[string]error{}
	_, refusals["auth.logout"] = conn.Logout(ctx, nil)
	_, refusals["sessionCapabilities.list"] = conn.ListSessions(ctx, nil)
	_, refusals["sessionCapabilities.delete"] = conn.DeleteSession(ctx,
		&acp.DeleteSessionRequest{SessionID: "stored-1"})
	_, _, refusals["sessionCapabilities.resume"] = conn.ResumeSession(ctx,
		&acp.ResumeSessionRequest{SessionID: "stored-1", Cwd: "/w"})
	_, refusals["sessionCapabilities.close"] = session.Close(ctx, nil)

	for capability, err := range refusals {
		if err == nil {
			t.Errorf("%s was called although the agent never advertised it", capability)
			continue
		}
		if !strings.Contains(err.Error(), capability) {
			t.Errorf("the refusal for %s was %v, which does not name the capability", capability, err)
		}
	}
}

// Closing a session cancels the work still running in it.
//
// The schema puts this on the agent as a MUST — "cancel any ongoing work related
// to the session (treat it as if session/cancel was called) and then free up any
// resources" — so the connection keeps it rather than leaving an application to
// remember that its own turn is still running.
func TestClosingASessionCancelsItsTurn(t *testing.T) {
	closed := make(chan acp.SessionID, 1)
	running := make(chan struct{})
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(ctx context.Context, _ *acp.AgentSession, _ *acp.PromptRequest) (*acp.PromptResponse, error) {
			// A turn that only ends because it was cancelled. Nothing here reports
			// the cancelled stop reason: the connection owes that.
			close(running)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		CloseSession: func(
			_ context.Context,
			session *acp.AgentSession,
			_ *acp.CloseSessionRequest,
		) (*acp.CloseSessionResponse, error) {
			closed <- session.ID()
			return &acp.CloseSessionResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	session := connectAndOpen(t, testClient(t), agent)
	prompted := make(chan *acp.PromptResponse, 1)
	failed := make(chan error, 1)
	go func() {
		response, err := session.Prompt(context.Background(), &acp.PromptParams{})
		if err != nil {
			failed <- err
			return
		}
		prompted <- response
	}()

	// The turn has to be the agent's before closing can be asked to cancel it;
	// otherwise this test would race the prompt onto the wire.
	<-running

	if _, err := session.Close(context.Background(), nil); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := <-closed; got != "sess-1" {
		t.Fatalf("the close handler saw session %q", got)
	}

	select {
	case response := <-prompted:
		if response.StopReason != acp.StopReasonCancelled {
			t.Fatalf("the turn ended with %q, want cancelled", response.StopReason)
		}
	case err := <-failed:
		t.Fatalf("closing the session failed its turn with %v, want the cancelled stop reason", err)
	}
}

// A closed session is forgotten, so a long-lived connection reclaims what it
// held. Nothing else reclaims one: the bound in limits.go exists because the
// population is otherwise one-way.
func TestClosingASessionFreesItsHandle(t *testing.T) {
	session := connectAndOpen(t, testClient(t), lifecycleAgent(t, nil))
	conn := session.Conn()
	ctx := context.Background()

	if _, err := session.Close(ctx, nil); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The identifier is the agent's to hand out again, and a handle for it is new.
	resumed, _, err := conn.ResumeSession(ctx, &acp.ResumeSessionRequest{SessionID: session.ID(), Cwd: "/w"})
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resumed == session {
		t.Fatal("the closed session's handle was handed out again")
	}
	if resumed.ID() != session.ID() {
		t.Fatalf("the new handle names %q, want %q", resumed.ID(), session.ID())
	}
}

// lifecycleAgent serves every optional session method, recording which.
func lifecycleAgent(t *testing.T, served chan<- string) *acp.Agent {
	t.Helper()

	record := func(method string) {
		if served != nil {
			served <- method
		}
	}
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
		Logout: func(context.Context, *acp.AgentConn, *acp.LogoutRequest) (*acp.LogoutResponse, error) {
			record("logout")
			return &acp.LogoutResponse{}, nil
		},
		ListSessions: func(
			_ context.Context,
			_ *acp.AgentConn,
			request *acp.ListSessionsRequest,
		) (*acp.ListSessionsResponse, error) {
			record("list")
			if cursor, paging := request.Cursor.Get(); paging {
				if cursor != "page-2" {
					t.Errorf("the second page asked for cursor %q", cursor)
				}
				return &acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
			}
			return &acp.ListSessionsResponse{
				Sessions:   []acp.SessionInfo{{SessionID: "stored-1", Cwd: "/w"}},
				NextCursor: acp.OptValue("page-2"),
			}, nil
		},
		DeleteSession: func(
			_ context.Context,
			session *acp.AgentSession,
			request *acp.DeleteSessionRequest,
		) (*acp.DeleteSessionResponse, error) {
			record("delete")
			if session.ID() != request.SessionID {
				t.Errorf("the delete handle names %q, request names %q", session.ID(), request.SessionID)
			}
			return &acp.DeleteSessionResponse{}, nil
		},
		ResumeSession: func(
			context.Context,
			*acp.AgentSession,
			*acp.ResumeSessionRequest,
		) (*acp.ResumeSessionResponse, error) {
			record("resume")
			return &acp.ResumeSessionResponse{}, nil
		},
		CloseSession: func(
			context.Context,
			*acp.AgentSession,
			*acp.CloseSessionRequest,
		) (*acp.CloseSessionResponse, error) {
			record("close")
			return &acp.CloseSessionResponse{}, nil
		},
		SetConfigOption: func(
			_ context.Context,
			_ *acp.AgentSession,
			request *acp.SetSessionConfigOptionRequest,
		) (*acp.SetSessionConfigOptionResponse, error) {
			record("set_config_option")
			if request.ConfigID != "verbosity" {
				t.Errorf("the option came through as %q", request.ConfigID)
			}
			return &acp.SetSessionConfigOptionResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

func hasValue[T any](option acp.Opt[T]) bool {
	_, present := option.Get()
	return present
}
