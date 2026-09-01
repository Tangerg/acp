package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// The three races turn cancellation used to have, each made deterministic.
//
// These are internal tests because they drive the connection's registration
// directly. Reproducing them through the public API would mean winning a race on
// purpose, which is the thing a test must not have to do.

// A permission request that arrives while a cancellation is running belongs to a
// turn that is already over.
//
// It used to escape: cancelPermissions took a snapshot and released the lock, so
// a request registered afterwards survived Cancel and a late user decision could
// still win. Now the session is closed to new registrations for as long as the
// cancellation is in progress.
func TestAPermissionRequestArrivingDuringCancellationIsAnsweredCancelled(t *testing.T) {
	conn := &ClientConn{}
	const session SessionID = "sess-1"

	first := jsonrpc2.Int64ID(1)
	if !conn.registerPermission(session, first) {
		t.Fatal("the first registration was refused before any cancellation")
	}

	// Cancelling closes the session. What was pending is claimed; what arrives
	// next is refused registration and answered by the caller.
	conn.turnsMu.Lock()
	conn.turns[session].cancelling = true
	conn.turnsMu.Unlock()

	if conn.registerPermission(session, jsonrpc2.Int64ID(2)) {
		t.Fatal("a permission request registered while the session was cancelling, so it would " +
			"have reached the application and its answer would then have been thrown away")
	}

	// And the session reopens when the turn ends, because a session is not
	// cancelled for ever: the next prompt is a new turn.
	generation := conn.turns[session].generation
	conn.endTurn(session, generation)
	if !conn.registerPermission(session, jsonrpc2.Int64ID(3)) {
		t.Fatal("the session stayed closed after the cancelled turn ended")
	}
}

// A cancellation that arrives before the turn's handler has started is applied to
// that turn, not lost.
//
// The read loop sees the prompt before the cancellation, because that is the order
// the peer sent them. The goroutines that serve them do not, so the turn is claimed
// where the ordering is still intact — and the request's context exists from the
// moment it was read, so cancelling it before its handler runs means the handler
// starts already cancelled.
//
// It used to be lost: the turn was claimed inside the prompt handler, so a
// cancellation that arrived first found nothing on record and did nothing, and the
// turn then ran to completion having never been told.
func TestACancellationBeforeTheHandlerStartsIsNotLost(t *testing.T) {
	conn := &AgentConn{conn: newConn(nil, nil, nil, nil)}
	const session SessionID = "sess-1"
	id := jsonrpc2.Int64ID(1)

	// What the read loop does with a request before spawning anything to serve it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn.conn.inflight[id] = cancel
	if entry := conn.registerInbound(rawRequest(id, methodSessionPrompt, promptParams)); !entry.dispatch {
		t.Fatal("the prompt was refused although the session had no turn")
	}

	conn.cancelTurn(session)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("the cancellation arrived before the handler and was lost; the turn would have run " +
			"to completion having never been told")
	}
}

// A cancellation is applied where the peer put it: after the messages it sent
// first.
//
// A client that cancels a turn answers the turn's outstanding permission requests
// before it sends the cancellation, because the agent is still waiting for those
// answers. Applying the cancellation on the read loop would overtake them, and the
// agent's own call would return "cancelled" instead of the cancelled outcome the
// client had already sent.
func TestACancellationDoesNotOvertakeTheAnswersSentBeforeIt(t *testing.T) {
	conn := &AgentConn{conn: newConn(nil, nil, nil, nil)}
	id := jsonrpc2.Int64ID(1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn.conn.inflight[id] = cancel
	conn.registerInbound(rawRequest(id, methodSessionPrompt, promptParams))

	// The read loop has now seen the cancellation. Nothing may have happened to
	// the turn yet: the queue in front of it has not been served.
	conn.registerInbound(rawNotification(methodSessionCancel, `{"sessionId":"sess-1"}`))
	select {
	case <-ctx.Done():
		t.Fatal("the cancellation was applied on the read loop, so it overtook every response the " +
			"client had already sent for this turn")
	default:
	}
}

// One turn per session, enforced against any peer and not only against this
// package's own client handle.
//
// The agent used to overwrite the recorded request, so a peer that sent two
// prompts got two turns and a later cancellation could name neither.
func TestASecondPromptForOneSessionIsRefused(t *testing.T) {
	conn := &AgentConn{conn: newConn(nil, nil, nil, nil)}
	const session SessionID = "sess-1"

	first := jsonrpc2.Int64ID(1)
	second := jsonrpc2.Int64ID(2)
	if !conn.claimTurn(session, first) {
		t.Fatal("the first prompt did not claim the turn")
	}
	if conn.claimTurn(session, second) {
		t.Fatal("a second prompt claimed the same session's turn, so a cancellation could name neither")
	}
	// The refused prompt does not end the turn it was refused for. If it did, the
	// second prompt failing would free the session out from under the first.
	conn.releaseTurn(session, second)
	if conn.claimTurn(session, second) {
		t.Fatal("a refused prompt released the turn the first prompt is holding")
	}

	// And the session is free again once the first turn ends.
	conn.releaseTurn(session, first)
	if !conn.claimTurn(session, second) {
		t.Fatal("the session stayed claimed after its turn ended")
	}
}

const promptParams = `{"sessionId":"sess-1","prompt":[]}`

// A second concurrent prompt is refused over the wire, with the reason, rather
// than silently taking over the session.
func TestASecondPromptOverTheWireIsRefused(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *NewSessionRequest) (*NewSessionResponse, error) {
			return &NewSessionResponse{SessionID: "sess-1"}, nil
		},
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			<-ctx.Done()
			return &PromptResponse{StopReason: StopReasonCancelled}, nil
		},
		Cancel: func(context.Context, *AgentSession, *CancelNotification) {},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	clientSide, agentSide := NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentConn, err := agent.Connect(ctx, agentSide)
	if err != nil {
		t.Fatalf("Agent.Connect: %v", err)
	}
	defer agentConn.Close() //nolint:errcheck // idempotent.

	stream, err := clientSide.Connect(ctx)
	if err != nil {
		t.Fatalf("transport.Connect: %v", err)
	}
	defer stream.Close() //nolint:errcheck // idempotent.

	// Initialize first and wait for the answer. Requests are served concurrently,
	// so writing all three at once would leave the order of the three responses
	// up to the scheduler — and this test is about the second prompt, not about
	// which handler finished first.
	if err := stream.Write(ctx, rawRequest(jsonrpc2.Int64ID(1), methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the initialize answer: %v", err)
	}

	// Now two prompts for one session. The first blocks; the second must be
	// refused rather than take the turn over.
	if err := stream.Write(ctx, rawRequest(jsonrpc2.Int64ID(2), methodSessionPrompt, `{"sessionId":"sess-1","prompt":[]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stream.Write(ctx, rawRequest(jsonrpc2.Int64ID(3), methodSessionPrompt, `{"sessionId":"sess-1","prompt":[]}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("the second prompt was answered %d (%s), want invalid request", code, code)
	}
}

func rawRequest(id jsonrpc.ID, method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{ID: id, Method: method, Params: json.RawMessage(params)}
}

func rawNotification(method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{Method: method, Params: json.RawMessage(params)}
}
