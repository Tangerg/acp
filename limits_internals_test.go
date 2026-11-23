package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

// documentedDefault is the bound Limits promises for a zero field.
const documentedDefault = 1024

// What a connection will hold for protocol work, and what it does when either
// side asks for more.
//
// These are internal tests because a bound is reached by feeding the read loop
// faster than anything drains it, which is a shape the public API exists to make
// impossible.
//
// Each drives a bound configured small rather than the default. The accounting is
// the same at either size, and stating the bound in the test is what makes it a
// test of the configured value instead of a test that 1024 is 1024.
const testBound = 8

// A peer that outruns delivery ends the connection rather than growing a backlog
// without limit.
func TestABacklogPastItsBoundEndsTheConnection(t *testing.T) {
	// Delivery is held inside the first notification's handler, so every message
	// behind it stays queued and nothing drains.
	reached := make(chan struct{})
	held := make(chan struct{})
	defer close(held)

	agent := blockingAgent(t, Limits{QueuedDeliveries: testBound}, reached, held)
	stream, agentConn := connectRawAgent(t, agent)
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), deadlockGuard)
	defer cancel()
	handshakeRaw(ctx, t, stream)

	if err := stream.Write(ctx, rawNotification(methodSessionCancel, `{"sessionId":"sess-1"}`)); err != nil {
		t.Fatalf("write the notification that holds delivery: %v", err)
	}
	<-reached

	// One past the bound, because the held notification has left the queue. A write
	// may fail part-way through, which is the connection ending as intended.
	for range testBound + 1 {
		if err := stream.Write(ctx, rawNotification(methodSessionCancel, `{"sessionId":"sess-1"}`)); err != nil {
			break
		}
	}

	waitEnded(ctx, t, agentConn, errTooManyQueued)
}

// A peer that opens more requests than this side will serve is refused, and the
// refusal is the one the read loop already ends the connection on — the same path
// a reused identifier takes, which TestReusingAnActiveRequestIDEndsTheConnection
// proves end to end.
//
// The bound itself is checked here rather than over the wire because filling it
// over the wire would race the delivery bound.
func TestTooManyInflightRequestsIsRefused(t *testing.T) {
	conn := newTestAgentConnWith(t, Limits{InflightRequests: testBound})

	for id := range testBound {
		if _, err := conn.requests.accept(t.Context(), inboundFor(jsonrpcTestID(id))); err != nil {
			t.Fatalf("request %d was refused below the bound: %v", id, err)
		}
	}

	_, err := conn.requests.accept(t.Context(), inboundFor(jsonrpcTestID(testBound)))
	if !errors.Is(err, errTooManyInflight) {
		t.Fatalf("the request past the bound was answered %v, want the in-flight bound", err)
	}

	// And finishing one makes room, so a peer that keeps up is never refused.
	completed := conn.requests.serving[jsonrpcTestID(0)]
	completed.settle()
	completed.finish()
	if _, err := conn.requests.accept(t.Context(), inboundFor(jsonrpcTestID(testBound))); err != nil {
		t.Fatalf("a request was refused after one finished: %v", err)
	}
}

// Sessions are not a connection-owned population: a handle holds only its
// identifier and connection, so a long-lived connection may open any number.
func TestManySessionsDoNotEndTheConnection(t *testing.T) {
	agent := blockingAgent(t, Limits{}, make(chan struct{}), make(chan struct{}))
	_, conn := connectRawAgent(t, agent)
	for index := range documentedDefault * 2 {
		if session := conn.session(SessionID(fmt.Sprintf("sess-%d", index))); session.Conn() != conn {
			t.Fatal("a session handle is not attached to its connection")
		}
	}
	if conn.life.ended() {
		t.Fatal("naming many sessions ended the connection")
	}
}

// A zero field takes the default for that field, so raising one bound does not
// silently drop the other three to nothing.
func TestAZeroLimitTakesItsDefault(t *testing.T) {
	resolved := Limits{QueuedDeliveries: 7}.resolve()

	if resolved.QueuedDeliveries != 7 {
		t.Fatalf("QueuedDeliveries resolved to %d, want the stated 7", resolved.QueuedDeliveries)
	}
	if resolved.InflightRequests != documentedDefault {
		t.Fatalf("InflightRequests resolved to %d, want the default %d",
			resolved.InflightRequests, documentedDefault)
	}
	if resolved.PendingCalls != documentedDefault {
		t.Fatalf("PendingCalls resolved to %d, want the default %d", resolved.PendingCalls, documentedDefault)
	}
	if resolved.OutstandingElicitations != documentedDefault {
		t.Fatalf("OutstandingElicitations resolved to %d, want the default %d",
			resolved.OutstandingElicitations, documentedDefault)
	}

	// And the connection reads the resolved value, so the bound in an error message
	// is the effective one rather than the field the caller left zero.
	conn := newTestAgentConnWith(t, Limits{})
	if conn.limits.QueuedDeliveries != documentedDefault {
		t.Fatalf("the link holds bound %d, want the resolved default %d",
			conn.limits.QueuedDeliveries, documentedDefault)
	}
}

// A negative bound is refused where it is stated, not on the message that would
// breach it: every push would refuse, and the first inbound message would end the
// connection for what is a configuration error rather than a peer's fault.
func TestANegativeLimitIsRefusedAtConstruction(t *testing.T) {
	_, clientErr := NewClient(&ClientConfig{
		SessionUpdate: func(context.Context, *ClientConn, *SessionNotification) {},
		RequestPermission: func(context.Context, *ClientConn, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
			return &RequestPermissionResponse{}, nil
		},
		Limits: Limits{QueuedDeliveries: -1},
	})
	if clientErr == nil {
		t.Fatal("NewClient accepted a negative bound")
	}

	_, agentErr := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			return &PromptResponse{}, nil
		},
		Limits: Limits{OutstandingElicitations: -1},
	})
	if agentErr == nil {
		t.Fatal("NewAgent accepted a negative bound")
	}
}

func jsonrpcTestID(n int) jsonrpc.ID { return jsonrpc.Int64ID(int64(n)) }

// blockingAgent serves nothing: the first handler to run reports that it arrived
// and then waits, which is how a test gets a peer that outruns this side.
func blockingAgent(t *testing.T, limits Limits, reached chan<- struct{}, held <-chan struct{}) *Agent {
	t.Helper()

	var once sync.Once
	arrive := func() {
		once.Do(func() { close(reached) })
		<-held
	}
	agent, err := NewAgent(&AgentConfig{
		Limits: limits,
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
			arrive()
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
			arrive()
			return &PromptResponse{StopReason: StopReasonEndTurn}, nil
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) { arrive() },
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

// handshakeRaw completes initialize from the hand-driven side, because an agent
// serves nothing else until it has.
func handshakeRaw(ctx context.Context, t *testing.T, stream Connection) {
	t.Helper()

	if err := stream.Write(ctx, rawCall(1, methodInitialize,
		`{"protocolVersion":1,"clientCapabilities":{}}`)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the initialize answer: %v", err)
	}
}

func waitEnded(ctx context.Context, t *testing.T, conn *AgentConn, want error) {
	t.Helper()

	select {
	case <-conn.life.released:
	case <-ctx.Done():
		t.Fatalf("the connection never ended; want %v", want)
	}
	if err := conn.life.terminal; !errors.Is(err, want) {
		t.Fatalf("the connection ended with %v, want %v", err, want)
	}
}
