package acp

import (
	"context"

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
	// A permission request that arrives while it is set is answered cancelled
	// without reaching the application: it belongs to a turn that is already over,
	// and asking a user about it would be asking about work nobody is waiting for.
	cancelled bool
	// sending counts the cancellations of this turn whose notification has not yet
	// gone out, which is a different fact from the turn having been cancelled and
	// was once the same field. The turn is not over while one is outstanding:
	// session/cancel names only a session, so a prompt that started before the
	// notification went out would be the turn the agent applies it to.
	sending int
	// pending is the permission requests this session is waiting on the user for.
	pending map[jsonrpc.ID]struct{}
}

// One turn per session, because session/cancel names none: a session with two
// turns would have no way to say which one a cancellation meant.
func (c *ClientConn) beginTurn(session SessionID) (uint64, bool) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turnFor(session)
	if turn.running || turn.sending > 0 {
		return 0, false
	}
	turn.generation++
	turn.running = true
	turn.cancelled = false
	return turn.generation, true
}

// A late answer to an abandoned turn must not end the turn that started after it,
// and without a generation the two are indistinguishable.
func (c *ClientConn) endTurn(session SessionID, generation uint64) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turns[session]
	if turn == nil || turn.generation != generation {
		return
	}
	turn.running = false
	// The turn is over, so a session that was cancelled is open to the next turn's
	// permission requests again. A session is not cancelled for ever.
	turn.cancelled = false
}

// Callers hold turnsMu.
func (c *ClientConn) turnFor(session SessionID) *clientTurn {
	if c.turns == nil {
		c.turns = make(map[SessionID]*clientTurn)
	}
	turn := c.turns[session]
	if turn == nil {
		turn = &clientTurn{pending: make(map[jsonrpc.ID]struct{})}
		c.turns[session] = turn
	}
	return turn
}

// Registration is on the read loop, before dispatch, so that a cancellation
// cannot slip between a request arriving and being registered.
func (c *ClientConn) registerPermission(session SessionID, id jsonrpc.ID) bool {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turnFor(session)
	if turn.cancelled {
		return false
	}
	turn.pending[id] = struct{}{}
	return true
}

func (c *ClientConn) forgetPermission(session SessionID, id jsonrpc.ID) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()
	if turn := c.turns[session]; turn != nil {
		delete(turn.pending, id)
	}
}

// beginCancel starts a cancellation and reports the turn it belongs to.
//
// The claim on the pending permission requests is synchronous and happens first —
// before the notification goes out and before Cancel returns. That ordering is
// what makes the race decidable: a user decision arriving afterwards finds the
// request already answered and is dropped, and a permission request arriving
// afterwards finds the session cancelling and is answered without ever reaching
// the application.
//
// The turn is held until endCancel, which is what stops the next prompt starting
// underneath a notification that has not gone out yet.
func (c *ClientConn) beginCancel(session SessionID) uint64 {
	c.turnsMu.Lock()
	turn := c.turnFor(session)
	turn.cancelled = true
	turn.sending++
	generation := turn.generation
	pending := make([]jsonrpc.ID, 0, len(turn.pending))
	for id := range turn.pending {
		pending = append(pending, id)
	}
	turn.pending = make(map[jsonrpc.ID]struct{})
	c.turnsMu.Unlock()

	// Written here rather than spawned, and before the caller sends the
	// cancellation: the agent is still waiting for these answers, and it should
	// have them before it is told the turn is over.
	for _, id := range pending {
		if write := c.answerCancelled(id); write != nil {
			write()
		}
	}
	return generation
}

// endCancel releases a cancellation once its notification is on the wire.
//
// Two concurrent cancellations of one turn each hold it, so the session reopens
// when the last of them has been sent rather than when the first returns.
func (c *ClientConn) endCancel(session SessionID, generation uint64) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()
	if turn := c.turns[session]; turn != nil && turn.generation == generation {
		turn.sending--
	}
}

// The claim is taken now, on whatever goroutine is asking, because it is what
// decides the race. Where the write goes is the caller's to say: cancelPermissions
// needs it on the wire before the cancellation notification, and the read loop
// needs it off the read loop.
func (c *ClientConn) answerCancelled(id jsonrpc.ID) func() {
	if !c.claimAnswer(id) {
		return nil
	}
	return func() {
		c.writeResponse(id, &RequestPermissionResponse{
			Outcome: &RequestPermissionOutcomeCancelled{},
		}, nil)
		c.cancelRequest(id)
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

	if !c.registerPermission(session, request.ID) {
		// The turn is being cancelled. Answering here rather than dispatching is
		// the difference between a dialog the user never sees and one they see and
		// whose answer is then thrown away.
		if write := c.answerCancelled(request.ID); write != nil {
			c.spawn(write)
		}
		return registration{}
	}
	return registration{
		dispatch: true,
		finished: func() { c.forgetPermission(session, request.ID) },
	}
}

// Registration has already happened, on the read loop, and forgetting it belongs
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
		return nil, newError(ErrorCodeInternalError, "the RequestPermission handler returned nothing")
	}
	return response, nil
}

// An agentTurn is what an agent knows about one session's current turn.
type agentTurn struct {
	// id is the prompt request being served, when one is.
	id      jsonrpc.ID
	running bool
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
		// This side may send once the client has been told what it agreed to.
		return registration{dispatch: true, answered: c.openOutbound}

	case methodSessionPrompt:
		session, ok := sessionOf(request)
		if !ok {
			// It will fail its own decode in the handler, with a better message.
			return registration{dispatch: true}
		}
		if !c.claimTurn(session, request.ID) {
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
		// Released when the handler returns and before the answer goes out: the
		// client may send the next prompt the moment it reads this one's answer.
		return registration{
			dispatch: true,
			finished: func() { c.releaseTurn(session, request.ID) },
		}

	default:
		return registration{dispatch: true}
	}
}

// The read loop needs the identifier and nothing else, and it must not spend the
// time to decode a whole prompt to get it.
func sessionOf(request *jsonrpc.Request) (SessionID, bool) {
	var params struct {
		SessionID SessionID `json:"sessionId"`
	}
	if err := decodeInto(request.Params, &params); err != nil {
		return "", false
	}
	return params.SessionID, true
}

// One turn per session, which is why the record is a single identifier rather
// than a set: session/cancel carries no turn identifier, so a session with two
// turns would have no way to say which one it meant.
func (c *AgentConn) claimTurn(session SessionID, id jsonrpc.ID) (claimed bool) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()
	if c.turns == nil {
		c.turns = make(map[SessionID]*agentTurn)
	}

	turn := c.turns[session]
	if turn == nil {
		turn = &agentTurn{}
		c.turns[session] = turn
	}
	if turn.running {
		return false
	}
	turn.id = id
	turn.running = true
	return true
}

// The identifier is checked because a second prompt that was refused must not
// release the turn the first one is holding.
func (c *AgentConn) releaseTurn(session SessionID, id jsonrpc.ID) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()
	if turn := c.turns[session]; turn != nil && turn.id == id {
		turn.running = false
	}
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
	c.turnsMu.Lock()
	turn := c.turns[session]
	if turn == nil || !turn.running {
		// A cancellation for a session with no turn on record. Nothing to do: the
		// turn is claimed on the read loop, so a prompt this cancellation follows
		// has already been claimed.
		c.turnsMu.Unlock()
		return
	}
	id := turn.id
	c.turnsMu.Unlock()

	c.cancelRequest(id)
}

// The turn was claimed on the read loop and is released by the request lifecycle,
// so what is left here is the handler.
func (c *AgentConn) prompt(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[PromptRequest](request)
	if err != nil {
		return nil, err
	}
	response, err := c.agent.config.Prompt(ctx, c.session(params.SessionID), params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, newError(ErrorCodeInternalError, "the Prompt handler returned nothing")
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
