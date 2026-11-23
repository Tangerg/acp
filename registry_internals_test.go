package acp

import (
	"context"
	"testing"
)

func TestClosedConnectionsLeaveTheirRegistryWithoutEnumeration(t *testing.T) {
	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *ClientConn, *SessionNotification) {},
		RequestPermission: denyingPermissionInternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "session"}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		clientTransport, agentTransport := NewInMemoryTransports()
		agentConn, err := agent.Connect(t.Context(), agentTransport)
		if err != nil {
			t.Fatal(err)
		}
		clientConn, err := client.Connect(t.Context(), clientTransport)
		if err != nil {
			_ = agentConn.Close()
			t.Fatal(err)
		}
		if err := clientConn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := clientConn.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := agentConn.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	client.conns.mu.Lock()
	clientRetained := len(client.conns.open)
	client.conns.mu.Unlock()
	agent.conns.mu.Lock()
	agentRetained := len(agent.conns.open)
	agent.conns.mu.Unlock()
	if clientRetained != 0 || agentRetained != 0 {
		t.Errorf("closed connections retained: client=%d agent=%d", clientRetained, agentRetained)
	}
}

func TestAConnectionThatEndsBeforeRegistrationIsNotRetained(t *testing.T) {
	life := newLifetime()
	life.endReading(nil, func() error { return nil })
	var connections registry[*lifetime]
	connections.add(life, life)
	if len(connections.open) != 0 {
		t.Error("registration resurrected an ended connection")
	}
}
