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
	conn := newTestClientConn()
	const session SessionID = "sess-1"
	generation, claimed := conn.turns.begin(session)
	if !claimed {
		t.Fatal("the turn was already claimed")
	}

	first := jsonrpc2.Int64ID(1)
	if got := conn.turns.registerPermission(session, first); got != permissionOwned {
		t.Fatal("the first registration was refused before any cancellation")
	}

	// Cancelling closes the session. What was pending is claimed; what arrives
	// next is refused registration and answered by the caller.
	cancellation := conn.turns.beginCancellation(session)

	if got := conn.turns.registerPermission(session, jsonrpc2.Int64ID(2)); got != permissionCancelled {
		t.Fatal("a permission request registered while the session was cancelling, so it would " +
			"have reached the application and its answer would then have been thrown away")
	}

	// The cancellation being on the wire is not the turn being over: until the
	// agent answers the prompt it may still ask, and the answer is still cancelled.
	conn.turns.finishCancellation(session, cancellation.generation)
	if got := conn.turns.registerPermission(session, jsonrpc2.Int64ID(3)); got != permissionCancelled {
		t.Fatal("a permission request reached the application after the cancellation was sent " +
			"but before the turn it cancelled had ended")
	}

	// And the session reopens when the turn ends, because a session is not
	// cancelled for ever: the next prompt is a new turn.
	conn.turns.complete(session, generation)
	if got := conn.turns.registerPermission(session, jsonrpc2.Int64ID(4)); got != permissionUnowned {
		t.Fatal("a permission request outside a turn was incorrectly attached to one")
	}
}

func TestACancellationOutsideATurnDoesNotClaimPermissionRequests(t *testing.T) {
	conn := newTestClientConn()
	const session SessionID = "sess-1"

	cancellation := conn.turns.beginCancellation(session)
	if got := conn.turns.registerPermission(session, jsonrpc2.Int64ID(1)); got != permissionUnowned {
		t.Fatal("session/cancel claimed a permission request although there was no prompt turn")
	}
	conn.turns.finishCancellation(session, cancellation.generation)
}

// A cancellation that arrives before the turn's handler has started is applied to
// that turn, not lost.
//
// Ordered delivery sees the prompt before the cancellation, because that is the order
// the peer sent them. The goroutines that serve them do not, so the turn is claimed
// where the ordering is still intact — and the request's context exists from the
// moment it was read, so cancelling it before its handler runs means the handler
// starts already cancelled.
//
// It used to be lost: the turn was claimed inside the prompt handler, so a
// cancellation that arrived first found nothing on record and did nothing, and the
// turn then ran to completion having never been told.
func TestACancellationBeforeTheHandlerStartsIsNotLost(t *testing.T) {
	conn := newTestAgentConn()
	const session SessionID = "sess-1"
	id := jsonrpc2.Int64ID(1)

	// What ordered admission does before spawning anything to serve the request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.requests.accept(id, cancel); err != nil {
		t.Fatalf("accepting the request: %v", err)
	}
	if entry := conn.register(rawRequest(id, methodSessionPrompt, promptParams)); !entry.dispatch {
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
// answers. Applying the cancellation as a preemptive read-side effect would overtake them, and the
// agent's own call would return "cancelled" instead of the cancelled outcome the
// client had already sent.
func TestACancellationDoesNotOvertakeTheAnswersSentBeforeIt(t *testing.T) {
	conn := newTestAgentConn()
	id := jsonrpc2.Int64ID(1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.requests.accept(id, cancel); err != nil {
		t.Fatalf("accepting the request: %v", err)
	}
	conn.register(rawRequest(id, methodSessionPrompt, promptParams))

	// Registration has now seen the cancellation. Nothing may have happened to
	// the turn yet: the queue in front of it has not been served.
	conn.register(rawNotification(methodSessionCancel, `{"sessionId":"sess-1"}`))
	select {
	case <-ctx.Done():
		t.Fatal("the cancellation was applied before ordered delivery, so it overtook every response the " +
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
	conn := newTestAgentConn()
	const session SessionID = "sess-1"

	first := jsonrpc2.Int64ID(1)
	second := jsonrpc2.Int64ID(2)
	if !conn.turns.claim(session, first) {
		t.Fatal("the first prompt did not claim the turn")
	}
	if conn.turns.claim(session, second) {
		t.Fatal("a second prompt claimed the same session's turn, so a cancellation could name neither")
	}
	// The refused prompt does not end the turn it was refused for. If it did, the
	// second prompt failing would free the session out from under the first.
	conn.turns.release(session, second)
	if conn.turns.claim(session, second) {
		t.Fatal("a refused prompt released the turn the first prompt is holding")
	}

	// And the session is free again once the first turn ends.
	conn.turns.release(session, first)
	if !conn.turns.claim(session, second) {
		t.Fatal("the session stayed claimed after its turn ended")
	}
}

// The agent can end model work at result commit, before the transport finishes
// writing it. A second prompt is unsafe before that point and safe afterwards;
// the old response's later settlement must not release the new turn.
func TestAPromptReopensOnlyAfterResultCommit(t *testing.T) {
	conn := newTestAgentConn()
	const session SessionID = "sess-1"
	first := jsonrpc2.Int64ID(1)
	second := jsonrpc2.Int64ID(2)

	entry := conn.register(rawRequest(first, methodSessionPrompt, promptParams))
	if !entry.dispatch || entry.served == nil || entry.settled == nil {
		t.Fatal("the first prompt did not retain its commit and settlement obligations")
	}
	if conn.turns.claim(session, second) {
		t.Fatal("a second turn started before the first result committed")
	}

	entry.served()
	if !conn.turns.claim(session, second) {
		t.Fatal("the first result committed but the session remained occupied")
	}
	entry.settled()
	if conn.turns.claim(session, first) {
		t.Fatal("settling the old response released the newer turn")
	}
}

func TestAgentTurnCompletionAndCancellationHaveOneWinner(t *testing.T) {
	const session SessionID = "sess-1"
	first := jsonrpc2.Int64ID(1)
	second := jsonrpc2.Int64ID(2)
	var turns agentTurns

	if !turns.claim(session, first) {
		t.Fatal("the first turn was not claimed")
	}
	if turns.commit(session, first) {
		t.Fatal("an uncancelled turn committed as cancelled")
	}
	if _, cancellable := turns.cancel(session); cancellable {
		t.Fatal("cancellation changed a result that had already committed")
	}
	turns.release(session, first)

	if !turns.claim(session, second) {
		t.Fatal("the second turn was not claimed")
	}
	if _, cancellable := turns.cancel(session); !cancellable {
		t.Fatal("cancellation did not claim an uncommitted result")
	}
	if !turns.commit(session, second) {
		t.Fatal("a cancellation that won before completion was not the committed result")
	}
}

const promptParams = `{"sessionId":"sess-1","prompt":[]}`

// The two connections these tests drive directly: a link with no transport, which
// is enough for registration and turn state and nothing that would need a peer.
func newTestAgentConn() *AgentConn { return newTestAgentConnWith(Limits{}) }

func newTestClientConn() *ClientConn {
	conn := &ClientConn{connection: newConnection()}
	conn.link = newLink(nil, conn, nil, Limits{})
	return conn
}

// A second concurrent prompt is refused over the wire, with the reason, rather
// than silently taking over the session.
func TestASecondPromptOverTheWireIsRefused(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: func(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
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
