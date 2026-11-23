package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Tangerg/acp/jsonrpc"
)

// A cancellation waits for the prompt it names to be written, and a caller's
// deadline does not abandon it once committed: the peers would otherwise
// disagree about the turn.
func TestSessionCancellationCannotOvertakeItsPromptWrite(t *testing.T) {
	for name, deadline := range map[string]bool{"ordered": false, "deadline": true} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := newTestClientConn(t, ClientConfig{})
				stream := &promptOrderStream{
					calls: conn.calls, entered: make(chan struct{}), resume: make(chan struct{}),
					written: make(chan string, 2),
				}
				conn.transport = stream
				session := &ClientSession{id: "session", conn: conn}
				prompt := make(chan error, 1)
				go func() { _, err := session.Prompt(t.Context(), nil); prompt <- err }()
				<-stream.entered
				ctx := t.Context()
				if deadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				}
				cancelled := make(chan error, 1)
				go func() { cancelled <- session.Cancel(ctx, nil) }()
				synctest.Wait()
				select {
				case method := <-stream.written:
					t.Errorf("%s overtook its prompt", method)
				default:
				}
				close(stream.resume)
				if err := <-prompt; err != nil {
					t.Fatal(err)
				}
				if err := <-cancelled; err != nil {
					t.Fatalf("a committed cancellation failed: %v", err)
				}
				for _, expected := range []string{methodSessionPrompt, methodSessionCancel} {
					select {
					case got := <-stream.written:
						if got != expected {
							t.Fatalf("wrote %s, want %s", got, expected)
						}
					default:
						t.Fatalf("did not write %s", expected)
					}
				}
				select {
				case extra := <-stream.written:
					t.Fatalf("unexpected write after cancellation: %s", extra)
				default:
				}
			})
		})
	}
}

func TestAlreadyCancelledCancelContextDoesNotChangeTheTurn(t *testing.T) {
	conn := newTestClientConn(t, ClientConfig{})
	turn, _ := conn.turns.begin(t.Context(), "session")
	defer turn.complete()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	session := &ClientSession{id: "session", conn: conn}
	if err := session.Cancel(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cancel returned %v", err)
	}
	if admission, _ := conn.turns.registerPermission(session.id, &inboundRequest{}); admission != permissionOwned {
		t.Fatal("a cancelled call changed the turn's permission decisions")
	}
}

type promptOrderStream struct {
	calls   *calls
	entered chan struct{}
	resume  chan struct{}
	written chan string
}

func (*promptOrderStream) Read(context.Context) (jsonrpc.Message, error) { return nil, io.EOF }
func (*promptOrderStream) Close() error                                  { return nil }

func (s *promptOrderStream) Write(_ context.Context, message jsonrpc.Message) error {
	request := message.(*jsonrpc.Request)
	if request.Method == methodSessionPrompt {
		close(s.entered)
		<-s.resume
	}
	s.written <- request.Method
	if request.IsCall() {
		s.calls.deliver(&jsonrpc.Response{ID: request.ID, Result: json.RawMessage(`{"stopReason":"end_turn"}`)})
	}
	return nil
}

// The three races turn cancellation used to have, each made deterministic.
//
// These are internal tests because they drive the connection's registration
// directly. Reproducing them through the public API would mean winning a race on
// purpose, which is the thing a test must not have to do.

// A permission request that arrives while a cancellation is running belongs to a
// turn that is already over, so it is answered cancelled without a dialog.
func TestAPermissionRequestArrivingDuringCancellationIsAnsweredCancelled(t *testing.T) {
	conn := newTestClientConn(t, ClientConfig{})
	const session SessionID = testSessionID
	turn, err := conn.turns.begin(t.Context(), session)
	if err != nil {
		t.Fatalf("the turn was not claimed: %v", err)
	}

	first := &inboundRequest{id: jsonrpc.Int64ID(1)}
	if got, _ := conn.turns.registerPermission(session, first); got != permissionOwned {
		t.Fatal("the first registration was refused before any cancellation")
	}

	// Cancelling closes the session. What was pending is claimed; what arrives
	// next is refused registration and answered by the caller.
	cancellation := conn.turns.beginCancellation(session)

	if got, _ := conn.turns.registerPermission(session, &inboundRequest{id: jsonrpc.Int64ID(2)}); got != permissionCancelled {
		t.Fatal("a permission request registered while the session was cancelling, so it would " +
			"have reached the application and its answer would then have been thrown away")
	}

	// The cancellation being on the wire is not the turn being over: until the
	// agent answers the prompt it may still ask, and the answer is still cancelled.
	cancellation.finish()
	if got, _ := conn.turns.registerPermission(session, &inboundRequest{id: jsonrpc.Int64ID(3)}); got != permissionCancelled {
		t.Fatal("a permission request reached the application after the cancellation was sent " +
			"but before the turn it cancelled had ended")
	}

	// And the session reopens when the turn ends, because a session is not
	// cancelled for ever: the next prompt is a new turn.
	turn.complete()
	if got, _ := conn.turns.registerPermission(session, &inboundRequest{id: jsonrpc.Int64ID(4)}); got != permissionUnowned {
		t.Fatal("a permission request outside a turn was incorrectly attached to one")
	}
}

func TestACancellationOutsideATurnDoesNotClaimPermissionRequests(t *testing.T) {
	conn := newTestClientConn(t, ClientConfig{})
	const session SessionID = testSessionID

	cancellation := conn.turns.beginCancellation(session)
	if got, _ := conn.turns.registerPermission(session, &inboundRequest{id: jsonrpc.Int64ID(1)}); got != permissionUnowned {
		t.Fatal("session/cancel claimed a permission request although there was no prompt turn")
	}
	cancellation.finish()
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
	conn := newTestAgentConn(t)
	const session SessionID = testSessionID
	id := jsonrpc.Int64ID(1)

	// What ordered admission does before spawning anything to serve the request.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	call, err := conn.requests.accept(ctx, inboundFor(id))
	if err != nil {
		t.Fatalf("accepting the request: %v", err)
	}
	if entry := conn.side.register(call, rawRequest(id, methodSessionPrompt, promptParams)); entry.automatic != nil {
		t.Fatalf("the prompt was answered automatically although the session had no turn: %v", entry.automatic.err)
	}

	conn.cancelTurn(session)
	select {
	case <-call.ctx.Done():
	default:
		t.Fatal("the cancellation arrived before the handler and was lost; the turn would have run " +
			"to completion having never been told")
	}
}

// A cancellation is applied where the peer put it: after the messages it sent
// first. A client answers a turn's outstanding permission requests before it
// cancels, and applying the cancellation when it is read would overtake those
// answers. So it is queued, and takes effect only when delivered.
func TestACancellationDoesNotOvertakeTheAnswersSentBeforeIt(t *testing.T) {
	conn := newTestAgentConn(t)
	id := jsonrpc.Int64ID(1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	call, err := conn.requests.accept(ctx, inboundFor(id))
	if err != nil {
		t.Fatalf("accepting the request: %v", err)
	}
	conn.side.register(call, rawRequest(id, methodSessionPrompt, promptParams))

	conn.admitRequest(rawNotification(methodSessionCancel, cancelParams))
	select {
	case <-call.ctx.Done():
		t.Fatal("the cancellation was applied when read, overtaking the responses queued before it")
	default:
	}
	for _, pending := range conn.queue.take() {
		conn.deliver(pending)
	}
	select {
	case <-call.ctx.Done():
	default:
		t.Fatal("the delivered cancellation did not cancel the turn")
	}
}

// One turn per session, enforced against any peer and not only against this
// package's own client handle.
//
// The agent used to overwrite the recorded request, so a peer that sent two
// prompts got two turns and a later cancellation could name neither.
func TestASecondPromptForOneSessionIsRefused(t *testing.T) {
	conn := newTestAgentConn(t)
	const session SessionID = testSessionID

	first := &inboundRequest{id: jsonrpc.Int64ID(1)}
	second := &inboundRequest{id: jsonrpc.Int64ID(2)}
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
	conn := newTestAgentConn(t)
	const session SessionID = testSessionID
	first := &inboundRequest{id: jsonrpc.Int64ID(1)}
	second := &inboundRequest{id: jsonrpc.Int64ID(2)}

	entry := conn.side.register(first, rawRequest(first.id, methodSessionPrompt, promptParams))
	if entry.automatic != nil || entry.hooks.chosen == nil || entry.hooks.finished == nil {
		t.Fatal("the first prompt did not retain its commit and settlement obligations")
	}
	if conn.turns.claim(session, second) {
		t.Fatal("a second turn started before the first result committed")
	}

	entry.hooks.chosen(nil, nil)
	if !conn.turns.claim(session, second) {
		t.Fatal("the first result committed but the session remained occupied")
	}
	entry.hooks.finished(nil)
	if conn.turns.claim(session, first) {
		t.Fatal("settling the old response released the newer turn")
	}
}

func TestAgentTurnCompletionAndCancellationHaveOneWinner(t *testing.T) {
	const session SessionID = testSessionID
	first := &inboundRequest{id: jsonrpc.Int64ID(1)}
	second := &inboundRequest{id: jsonrpc.Int64ID(2)}
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

// The two connections these tests drive directly: no transport, which is enough
// for registration and turn state, with an agreement already settled.
func newTestAgentConn(t *testing.T) *AgentConn {
	t.Helper()
	conn := newTestAgentConnWith(t, Limits{})
	conn.negotiation.accept(Negotiated{})
	conn.negotiation.open()
	return conn
}

func newTestClientConn(t *testing.T, config ClientConfig) *ClientConn {
	t.Helper()
	conn := newTestClientConnWith(t, config)
	conn.negotiation.accept(Negotiated{})
	conn.negotiation.open()
	return conn
}

// A second concurrent prompt is refused over the wire, with the reason, rather
// than silently taking over the session.
func TestASecondPromptOverTheWireIsRefused(t *testing.T) {
	agent, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt: func(ctx context.Context, _ *AgentSession, _ *PromptRequest) (*PromptResponse, error) {
			<-ctx.Done()
			return &PromptResponse{StopReason: StopReasonCancelled}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	clientSide, agentSide := NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), deadlockGuard)
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
	if err := stream.Write(ctx, rawRequest(jsonrpc.Int64ID(1), methodInitialize, `{"protocolVersion":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := stream.Read(ctx); err != nil {
		t.Fatalf("read the initialize answer: %v", err)
	}

	// Now two prompts for one session. The first blocks; the second must be
	// refused rather than take the turn over.
	if err := stream.Write(ctx, rawRequest(jsonrpc.Int64ID(2), methodSessionPrompt, promptParams)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := stream.Write(ctx, rawRequest(jsonrpc.Int64ID(3), methodSessionPrompt, promptParams)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := readRawErrorCode(t, stream); code != ErrorCodeInvalidRequest {
		t.Fatalf("the second prompt was answered %d (%s), want invalid request", code, code)
	}
}

func rawRequest(id jsonrpc.ID, method, params string) *jsonrpc.Request {
	return &jsonrpc.Request{ID: id, Method: method, Params: json.RawMessage(params)}
}
