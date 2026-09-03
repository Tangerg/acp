package acp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Tangerg/acp"
)

// The vocabulary the tests are written in. A helper belongs here once a second
// file needs it; one that only ever serves a single test stays beside that test.

// text encodes a value the way it would go on the wire, so that a test can ask
// one question of a whole tree.
func text(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", reflect.TypeOf(value), err)
	}
	return string(encoded)
}

func count[T any](seq func(func(T) bool)) int {
	total := 0
	for range seq {
		total++
	}
	return total
}

func denyingPermission(
	_ context.Context,
	_ *acp.RequestPermissionRequest,
) (*acp.RequestPermissionResponse, error) {
	return &acp.RequestPermissionResponse{Outcome: &acp.RequestPermissionOutcomeCancelled{}}, nil
}

func testClient(t *testing.T) *acp.Client {
	t.Helper()
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func testAgent(
	t *testing.T,
	prompt func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error),
) *acp.Agent {
	t.Helper()
	if prompt == nil {
		prompt = func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
	}
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: prompt,
		Cancel: func(context.Context, *acp.AgentSession, *acp.CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

// connectPeers wires a client and an agent together over one in-memory pair and
// returns the client's end, before any session exists.
func connectPeers(t *testing.T, client *acp.Client, agent *acp.Agent) *acp.ClientConn {
	t.Helper()
	clientSide, agentSide := acp.NewInMemoryTransports()
	ctx := context.Background()

	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	conn, err := client.Connect(ctx, clientSide)
	if err != nil {
		t.Fatalf("Client.Connect: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = agentConn.Close()
	})
	return conn
}

// connectAndOpen wires a client and an agent together and opens one session.
func connectAndOpen(t *testing.T, client *acp.Client, agent *acp.Agent) *acp.ClientSession {
	t.Helper()
	conn := connectPeers(t, client, agent)

	session, _, err := conn.NewSession(context.Background(), &acp.NewSessionRequest{Cwd: "/w"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return session
}
