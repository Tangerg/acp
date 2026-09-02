package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// What a connection will hold on a peer's behalf, and what it does when the peer
// asks for more.
//
// These are internal tests because a bound is reached by feeding the read loop
// faster than anything drains it, which is a shape the public API exists to make
// impossible.

// A peer that outruns delivery ends the connection rather than growing a backlog
// without limit.
func TestABacklogPastItsBoundEndsTheConnection(t *testing.T) {
	// Delivery is held inside the first notification's handler, so every message
	// behind it stays queued and nothing drains.
	reached := make(chan struct{})
	held := make(chan struct{})
	defer close(held)

	stream, agentConn := connectRawAgent(t, blockingAgent(t, reached, held))
	defer agentConn.Close() //nolint:errcheck // idempotent.

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handshakeRaw(ctx, t, stream)

	if err := stream.Write(ctx, rawNotification(methodSessionCancel, `{"sessionId":"sess-1"}`)); err != nil {
		t.Fatalf("write the notification that holds delivery: %v", err)
	}
	<-reached

	// One past the bound, because the held notification has left the queue. A write
	// may fail part-way through, which is the connection ending as intended.
	for range maxQueuedDeliveries + 1 {
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
// The bound itself is checked here rather than over the wire because it is the
// same size as the delivery bound: a wire test would race whichever filled first.
func TestTooManyInflightRequestsIsRefused(t *testing.T) {
	conn := newTestAgentConn()

	for id := range maxInflightRequests {
		_, cancel := context.WithCancel(context.Background())
		if err := conn.requests.accept(jsonrpcTestID(id), cancel); err != nil {
			t.Fatalf("request %d was refused below the bound: %v", id, err)
		}
	}

	_, cancel := context.WithCancel(context.Background())
	err := conn.requests.accept(jsonrpcTestID(maxInflightRequests), cancel)
	if !errors.Is(err, errTooManyInflight) {
		t.Fatalf("the request past the bound was answered %v, want the in-flight bound", err)
	}

	// And finishing one makes room, so a peer that keeps up is never refused.
	conn.requests.release(jsonrpcTestID(0))
	_, cancel = context.WithCancel(context.Background())
	if err := conn.requests.accept(jsonrpcTestID(maxInflightRequests), cancel); err != nil {
		t.Fatalf("a request was refused after one finished: %v", err)
	}
}

// A peer that keeps naming fresh sessions without closing them ends the
// connection. Serving session/close is what reclaims an agent's cached entry;
// merely holding a handle on the application's side is not what makes the
// population grow.
func TestTooManySessionsEndsTheConnection(t *testing.T) {
	// A real transport, because this bound ends the connection and ending one
	// closes what it was reading.
	_, conn := connectRawAgent(t, blockingAgent(t, make(chan struct{}), make(chan struct{})))

	var last *AgentSession
	for index := range maxSessionsPerConnection + 1 {
		last = conn.session(SessionID(fmt.Sprintf("sess-%d", index)))
	}

	// The handle past the bound is still real, so whatever was mid-flight fails on
	// the connection ending rather than on a nil pointer.
	if last == nil || last.Conn() != conn {
		t.Fatal("the handle minted past the bound is not attached to this connection")
	}
	if !conn.ended() {
		t.Fatal("naming more sessions than the bound left the connection open")
	}
	if err := conn.life.terminal; !errors.Is(err, errTooManySessions) {
		t.Fatalf("the connection ended with %v, want the session bound", err)
	}
}

// A handle already minted is returned again rather than counted again, so a peer
// reusing its sessions never approaches the bound.
func TestRepeatingASessionDoesNotConsumeTheBound(t *testing.T) {
	_, conn := connectRawAgent(t, blockingAgent(t, make(chan struct{}), make(chan struct{})))

	first := conn.session("sess-1")
	for range maxSessionsPerConnection * 2 {
		if again := conn.session("sess-1"); again != first {
			t.Fatal("one identifier produced two handles, so the one-prompt rule holds for neither")
		}
	}
	if conn.ended() {
		t.Fatal("repeating one session ended the connection")
	}
}

func jsonrpcTestID(n int) jsonrpc.ID { return jsonrpc2.Int64ID(int64(n)) }

// blockingAgent serves nothing: the first handler to run reports that it arrived
// and then waits, which is how a test gets a peer that outruns this side.
func blockingAgent(t *testing.T, reached chan<- struct{}, held <-chan struct{}) *Agent {
	t.Helper()

	var once sync.Once
	arrive := func() {
		once.Do(func() { close(reached) })
		<-held
	}
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *NewSessionRequest) (*NewSessionResponse, error) {
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

	for !conn.ended() {
		select {
		case <-ctx.Done():
			t.Fatalf("the connection never ended; want %v", want)
		case <-time.After(time.Millisecond):
		}
	}
	if err := conn.life.terminal; !errors.Is(err, want) {
		t.Fatalf("the connection ended with %v, want %v", err, want)
	}
}
