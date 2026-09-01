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
	// cancelling is set for as long as a cancellation is in progress. A permission
	// request that arrives during it is answered cancelled without reaching the
	// application: it belongs to a turn that is already over, and asking a user
	// about it would be asking about work nobody is waiting for.
	cancelling bool
	// pending is the permission requests this session is waiting on the user for.
	pending map[jsonrpc.ID]struct{}
}

// beginTurn claims a session's turn, and reports the generation it claimed.
//
// One turn per session, because session/cancel names none: a session with two
// turns would have no way to say which one a cancellation meant.
func (c *ClientConn) beginTurn(session SessionID) (uint64, bool) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turnFor(session)
	if turn.running {
		return 0, false
	}
	turn.generation++
	turn.running = true
	turn.cancelling = false
	return turn.generation, true
}

// endTurn ends a turn, if the turn named is still the one running.
//
// The generation is what makes that check possible. A late answer to an abandoned
// turn must not end the turn that started after it, and without a generation the
// two are indistinguishable.
func (c *ClientConn) endTurn(session SessionID, generation uint64) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turns[session]
	if turn == nil || turn.generation != generation {
		return
	}
	turn.running = false
	// The turn is over, so a session that was cancelling is open to the next
	// turn's permission requests again. A session is not cancelled for ever.
	turn.cancelling = false
}

// turnFor returns a session's turn state, creating it once. Callers hold turnsMu.
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

// registerPermission claims a permission request for a session, and reports
// whether the turn is still live.
//
// It runs on the read loop, before the request is dispatched, so a cancellation
// cannot slip between a request arriving and being registered.
func (c *ClientConn) registerPermission(session SessionID, id jsonrpc.ID) bool {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()

	turn := c.turnFor(session)
	if turn.cancelling {
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

// cancelPermissions answers every pending permission request for a session with
// the cancelled outcome, and closes the session to new ones while it does.
//
// The claim is synchronous and happens first — before the notification goes out
// and before Cancel returns. That ordering is what makes the race decidable: a
// user decision arriving afterwards finds the request already answered and is
// dropped, and a permission request arriving afterwards finds the session
// cancelling and is answered without ever reaching the application.
func (c *ClientConn) cancelPermissions(session SessionID) {
	c.turnsMu.Lock()
	turn := c.turnFor(session)
	turn.cancelling = true
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
}

// answerCancelled claims one permission request for the cancelled outcome and
// returns the write that delivers it, or nil if something has already answered.
//
// The claim is taken now, on whatever goroutine is asking, because it is what
// decides the race. Where the write goes is the caller's to say: cancelPermissions
// needs it on the wire before the cancellation notification, and the read loop
// needs it off the read loop.
func (c *ClientConn) answerCancelled(id jsonrpc.ID) func() {
	write := c.conn.answer(id, &RequestPermissionResponse{
		Outcome: &RequestPermissionOutcomeCancelled{},
	}, nil)
	if write == nil {
		return nil
	}
	return func() {
		write()
		c.conn.cancelRequest(id)
	}
}

// registerInbound runs on the read loop for every call a client receives.
func (c *ClientConn) registerInbound(request *jsonrpc.Request) registration {
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
			c.conn.spawn(write)
		}
		return registration{}
	}
	return registration{
		dispatch: true,
		finished: func() { c.forgetPermission(session, request.ID) },
	}
}

// requestPermission serves session/request_permission.
//
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

// registerInbound runs on the read loop for every call an agent receives.
//
// This is where the ordering the peer intended becomes the ordering this
// connection has. A prompt is claimed here rather than in its handler, so a
// cancellation that follows it on the wire finds the turn on record even though
// the handler that serves it may not have started.
//
// Only the claim belongs here. The cancellation itself is an effect on work
// already in progress, and doing it here would let it overtake every message read
// before it — including the responses the peer sent first, which is exactly the
// order a cancelled turn depends on.
func (c *AgentConn) registerInbound(request *jsonrpc.Request) registration {
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
			if write := c.conn.answer(request.ID, nil, refusal); write != nil {
				c.conn.spawn(write)
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

// sessionOf reads the session a request names, without decoding the rest of it.
//
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

// claimTurn records the request carrying a session's turn.
//
// One turn per session, which is what makes this a single identifier rather than
// a set: session/cancel carries no turn identifier, so a session with two turns
// would have no way to say which one it meant.
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

// releaseTurn ends a turn, if the request named is the one holding it. A second
// prompt that was refused must not release the first prompt's turn.
func (c *AgentConn) releaseTurn(session SessionID, id jsonrpc.ID) {
	c.turnsMu.Lock()
	defer c.turnsMu.Unlock()
	if turn := c.turns[session]; turn != nil && turn.id == id {
		turn.running = false
	}
}

// cancelTurn cancels a session's turn and the work descending from it.
//
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

	c.conn.cancelRequest(id)
}

// prompt serves session/prompt.
//
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

// cancel serves session/cancel.
//
// It runs from the ordered queue, which is where the cancellation belongs: the
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
