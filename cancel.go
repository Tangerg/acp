package acp

import (
	"context"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// Turn cancellation, and who owns each obligation.
//
// There are two cancellations and they are not the same thing. $/cancel_request
// cancels one JSON-RPC request; session/cancel cancels a turn and obliges the
// agent to finish the outstanding session/prompt with the cancelled stop reason.
// Cancelling a Prompt's context does the first; [ClientSession.Cancel] does the
// second.
//
// Separating them leaves obligations that cannot be an application's problem, and
// this file is where the connection takes them:
//
//   - A client that cancels a turn must answer every pending
//     session/request_permission for that session with the cancelled outcome —
//     while the user's handler may still be blocked on a dialog, and including
//     one that arrives while the cancellation is still running.
//   - An agent that receives a cancellation must cancel the turn's context and
//     the work descending from it, and nothing else — including a cancellation
//     that arrives before the handler for the turn it names has started.
//
// Both obligations are races, and both are settled by one piece of per-session
// state with one lock rather than by ordering two independent maps.

// A clientTurn is what a client knows about one session's current turn.
//
// The turn and the caller waiting on it are different lifetimes. A caller that
// gives up on Prompt has stopped waiting; the agent still owes an answer, and
// until that answer is observed this session has a turn. Recording otherwise let
// a second prompt start while the first was still running, and left a cancelled
// session cancelling for ever once the caller had walked away.
type clientTurn struct {
	// generation counts the turns this session has had, so that one turn ending
	// can be told from the next beginning. A boolean cannot: the answer to a turn
	// nobody is waiting for arrives with nothing to say which turn it was.
	generation uint64
	// running is set from the prompt being sent to the agent's answer being
	// observed, which is not the same as this caller's patience.
	running bool
	// cancelled is set once this turn has been cancelled and cleared when it ends.
	// A permission request owned by this still-running turn is then answered
	// cancelled without reaching the application: asking a user would ask about
	// work nobody is waiting for. Requests outside a turn are not owned here.
	cancelled bool
	// cancelling counts the cancellations of this turn whose notification has not yet
	// gone out, which is a different fact from the turn having been cancelled and
	// was once the same field. The turn is not over while one is outstanding:
	// session/cancel names only a session, so a prompt that started before the
	// notification went out would be the turn the agent applies it to.
	cancelling int
	// pending is the permission requests this session is waiting on the user for.
	pending map[jsonrpc.ID]struct{}
}

// clientTurns owns every state transition whose meaning depends on
// session/cancel naming a session rather than a turn.
type clientTurns struct {
	mu       sync.Mutex
	sessions map[SessionID]*clientTurn
}

type clientCancellation struct {
	generation uint64
	pending    []jsonrpc.ID
}

type permissionAdmission uint8

const (
	permissionUnowned permissionAdmission = iota
	permissionOwned
	permissionCancelled
)

func (t *clientTurns) begin(session SessionID) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	turn := t.session(session)
	if turn.running || turn.cancelling > 0 {
		return 0, false
	}
	turn.generation++
	turn.running = true
	turn.cancelled = false
	return turn.generation, true
}

// A late answer to an abandoned turn must not end the turn that started after it,
// and without a generation the two are indistinguishable.
func (t *clientTurns) complete(session SessionID, generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	turn := t.sessions[session]
	if turn == nil || turn.generation != generation {
		return
	}
	turn.running = false
	if turn.cancelling == 0 {
		turn.cancelled = false
	}
}

// Callers hold t.mu because returning mutable turn state is intentionally not
// part of the aggregate's API.
func (t *clientTurns) session(id SessionID) *clientTurn {
	if t.sessions == nil {
		t.sessions = make(map[SessionID]*clientTurn)
	}
	turn := t.sessions[id]
	if turn == nil {
		turn = &clientTurn{pending: make(map[jsonrpc.ID]struct{})}
		t.sessions[id] = turn
	}
	return turn
}

// Registration is on the ordered delivery loop, before dispatch, so that a cancellation
// cannot slip between a request arriving and being registered.
func (t *clientTurns) registerPermission(session SessionID, id jsonrpc.ID) permissionAdmission {
	t.mu.Lock()
	defer t.mu.Unlock()

	turn := t.sessions[session]
	if turn == nil || !turn.running {
		return permissionUnowned
	}
	if turn.cancelled {
		return permissionCancelled
	}
	turn.pending[id] = struct{}{}
	return permissionOwned
}

func (t *clientTurns) forgetPermission(session SessionID, id jsonrpc.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if turn := t.sessions[session]; turn != nil {
		delete(turn.pending, id)
	}
}

// The claim on the pending permission requests is synchronous and happens first —
// before the notification goes out and before Cancel returns. That ordering is
// what makes the race decidable: a user decision arriving afterwards finds the
// request already answered and is dropped, and a permission request arriving
// afterwards finds the session cancelling and is answered without ever reaching
// the application.
//
// The turn is held until endCancel, which is what stops the next prompt starting
// underneath a notification that has not gone out yet.
func (t *clientTurns) beginCancellation(session SessionID) clientCancellation {
	t.mu.Lock()
	defer t.mu.Unlock()

	turn := t.session(session)
	turn.cancelled = true
	turn.cancelling++
	pending := make([]jsonrpc.ID, 0, len(turn.pending))
	for id := range turn.pending {
		pending = append(pending, id)
	}
	turn.pending = make(map[jsonrpc.ID]struct{})
	return clientCancellation{generation: turn.generation, pending: pending}
}

// Two concurrent cancellations of one turn each hold it, so the session reopens
// when the last of them has been sent rather than when the first returns.
func (t *clientTurns) finishCancellation(session SessionID, generation uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if turn := t.sessions[session]; turn != nil && turn.generation == generation {
		turn.cancelling--
		if turn.cancelling == 0 && !turn.running {
			turn.cancelled = false
		}
	}
}

// The claim is taken now, on whatever goroutine is asking, because it is what
// decides the race. Where the write goes is the caller's to say: cancelPermissions
// needs it on the wire before the cancellation notification, and ordered
// delivery needs the write off its own loop.
func (c *ClientConn) answerCancelled(id jsonrpc.ID) func() {
	if !c.claimAnswer(id) {
		return nil
	}
	return func() {
		c.writeResponse(id, &RequestPermissionResponse{
			Outcome: &RequestPermissionOutcomeCancelled{},
		}, nil)
		c.interruptRequest(id)
	}
}

func (c *ClientConn) register(request *jsonrpc.Request) registration {
	if request.Method != methodSessionRequestPermission {
		return registration{dispatch: true}
	}
	session, ok := sessionOf(request)
	if !ok {
		// It will fail its own decode in the handler, with a better message.
		return registration{dispatch: true}
	}

	switch c.turns.registerPermission(session, request.ID) {
	case permissionOwned:
	case permissionCancelled:
		// The turn is being cancelled. Answering here rather than dispatching is
		// the difference between a dialog the user never sees and one they see and
		// whose answer is then thrown away.
		if write := c.answerCancelled(request.ID); write != nil {
			c.spawn(write)
		}
		return registration{}
	case permissionUnowned:
		// The published grammar does not make permission requests children of a
		// prompt. Without an active turn there is nothing for session/cancel to
		// claim, but the request itself is still valid and cancellable through
		// $/cancel_request.
		return registration{dispatch: true}
	}
	return registration{
		dispatch: true,
		settled:  func() { c.turns.forgetPermission(session, request.ID) },
	}
}

// Registration has already happened in wire order, and forgetting it belongs
// to the request lifecycle. If the turn was cancelled between then and now the
// request has been answered, and the claim in respond is what stops this answer
// being a second one.
func (c *ClientConn) requestPermission(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[RequestPermissionRequest](request)
	if err != nil {
		return nil, err
	}

	response, err := c.client.config.RequestPermission(ctx, params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

// An agentTurn is what an agent knows about one session's current turn.
type agentTurn struct {
	// id is the prompt request being served, when one is.
	id jsonrpc.ID
	// cancelled makes session/cancel a semantic PromptResponse outcome; the
	// request context alone cannot distinguish it from connection shutdown.
	cancelled bool
	// committed means the handler has chosen the semantic result. A cancellation
	// that linearizes afterwards is too late to change it, even if the transport
	// is still writing the response.
	committed bool
}

// agentTurns owns the request identifier targeted by session/cancel. Keeping the
// map behind this object prevents connection code from observing a half-updated
// turn.
type agentTurns struct {
	mu       sync.Mutex
	sessions map[SessionID]agentTurn
}

func (t *agentTurns) claim(session SessionID, id jsonrpc.ID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = make(map[SessionID]agentTurn)
	}
	if turn, running := t.sessions[session]; running && !turn.committed {
		return false
	}
	t.sessions[session] = agentTurn{id: id}
	return true
}

func (t *agentTurns) release(session SessionID, id jsonrpc.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if running && turn.id == id {
		delete(t.sessions, session)
	}
}

func (t *agentTurns) cancel(session SessionID) (jsonrpc.ID, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if running && !turn.committed {
		turn.cancelled = true
		t.sessions[session] = turn
		return turn.id, true
	}
	return turn.id, false
}

// Deciding result-against-cancellation inside the aggregate is what makes it one
// decision: checking a flag and then writing outside would leave a window in
// which both a normal result and a cancellation appeared to win.
func (t *agentTurns) commit(session SessionID, id jsonrpc.ID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if !running || turn.id != id {
		return false
	}
	turn.committed = true
	t.sessions[session] = turn
	return turn.cancelled
}

// This is where the ordering the peer intended becomes the ordering this
// connection has. A prompt is claimed here rather than in its handler, so a
// cancellation that follows it on the wire finds the turn on record even though
// the handler that serves it may not have started.
//
// Only the claim belongs here. The cancellation itself is an effect on work
// already in progress, and doing it here would let it overtake every message read
// before it — including the responses the peer sent first, which is exactly the
// order a cancelled turn depends on.
func (c *AgentConn) register(request *jsonrpc.Request) registration {
	switch request.Method {
	case methodInitialize:
		// The attempt is queued in wire order, not claimed in a concurrent handler.
		// A later request waits for the preceding answer to settle, then either owns
		// the still-idle negotiation or observes the accepted agreement.
		attempt := c.handshake.registerAttempt()
		return registration{
			dispatch: true,
			admit:    attempt.await,
			settled:  attempt.settle,
			answered: attempt.publish,
		}

	case methodSessionPrompt:
		session, ok := sessionOf(request)
		if !ok {
			// It will fail its own decode in the handler, with a better message.
			return registration{dispatch: true}
		}
		if !c.turns.claim(session, request.ID) {
			// Refused here rather than in the handler, because the handler is not
			// where the ordering is intact. The rule is the one the client handle
			// keeps locally, kept here against any peer.
			refusal := newError(ErrorCodeInvalidRequest,
				"session %s already has a prompt in flight, and session/cancel names no turn", session)
			if c.claimAnswer(request.ID) {
				c.spawn(func() { c.writeResponse(request.ID, nil, refusal) })
			}
			return registration{}
		}
		// Released only after the answer write settles. The peer may send the next
		// prompt after it reads that answer, while one it pipelines before then must
		// still find this turn in flight.
		return registration{
			dispatch: true,
			served:   func() { c.turns.commit(session, request.ID) },
			settled:  func() { c.turns.release(session, request.ID) },
		}

	default:
		return registration{dispatch: true}
	}
}

// Admission needs the identifier and nothing else, and must not spend the time to
// decode a whole prompt to get it.
func sessionOf(request *jsonrpc.Request) (SessionID, bool) {
	var params struct {
		SessionID *SessionID `json:"sessionId"`
	}
	if err := decodeInto(request.Params, &params); err != nil || params.SessionID == nil {
		return "", false
	}
	return *params.SessionID, true
}

// It does not answer the prompt. The protocol says what the answer is — the
// cancelled stop reason — and it is the agent's handler that owes it: the client
// is still waiting on that response, and the agent may have final tool-call
// updates to send before it. Other sessions and unrelated calls are untouched.
//
// The turn's handler may not have started yet, and that is not a problem the turn
// has to solve: the request's context is created when the request is read, so
// cancelling it now means the handler starts already cancelled.
func (c *AgentConn) cancelTurn(session SessionID) {
	id, cancellable := c.turns.cancel(session)
	if !cancellable {
		// Nothing to interrupt: either no turn is on record, or its handler already
		// committed the result. The turn is claimed in wire order, so a prompt this
		// cancellation follows cannot be missing merely because its handler has not
		// started.
		return
	}
	c.interruptRequest(id)
}

// The turn was claimed in wire order and is released by the request lifecycle,
// so what is left here is the handler.
func (c *AgentConn) prompt(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[PromptRequest](request)
	if err != nil {
		// Ordered admission may have claimed the session from the small prefix it
		// could decode. Once the full payload is known invalid, commit that failure
		// so a later cancellation cannot reinterpret it as a valid cancelled turn.
		if session, ok := sessionOf(request); ok {
			c.turns.commit(session, request.ID)
		}
		return nil, err
	}
	response, err := c.agent.config.Prompt(ctx, c.session(params.SessionID), params)
	if c.turns.commit(params.SessionID, request.ID) {
		// session/cancel is a semantic turn result, not a failed JSON-RPC call.
		// This boundary catches abort errors from model and tool libraries so they
		// cannot leak as -32603, which the protocol explicitly warns against.
		return &PromptResponse{StopReason: StopReasonCancelled}, nil
	}
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

// This runs from the ordered queue, which is where the cancellation belongs: the
// responses the client sent before it — the cancelled permission outcomes it owed
// the turn — are delivered first, and the agent's own calls return their answers
// rather than the cancellation that was chasing them.
func (c *AgentConn) cancel(ctx context.Context, request *jsonrpc.Request) error {
	params, err := decodeParams[CancelNotification](request)
	if err != nil {
		return err
	}
	c.cancelTurn(params.SessionID)
	c.agent.config.Cancel(ctx, c.session(params.SessionID), params)
	return nil
}
