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
// Separating them leaves obligations that cannot be an application's problem:
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
// state with one lock rather than by ordering two independent maps. That state is
// what this file holds. The connection methods that discharge the obligations
// live with the rest of their own type, in client.go and agent.go.

// clientTurns owns the active turn and cancellation writes for each session.
// A turn record is its identity; completed turns need no persistent generation
// counter, and delayed work keeps the old record rather than finding a new turn
// by its session ID.
type clientTurns struct {
	mu       sync.Mutex
	sessions map[SessionID]*clientTurn
}

type clientTurn struct {
	owner      *clientTurns
	session    SessionID
	running    bool
	sending    chan struct{}
	cancelled  bool
	cancelling int
	pending    map[*inboundRequest]struct{}
}

type clientCancellation struct {
	turn    *clientTurn
	pending []*inboundRequest
}

type permissionAdmission uint8

const (
	permissionUnowned permissionAdmission = iota
	permissionOwned
	permissionCancelled
)

func (t *clientTurns) begin(session SessionID) (*clientTurn, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.session(session)
	if turn.running || turn.cancelling > 0 {
		return nil, false
	}
	turn.running = true
	turn.sending = make(chan struct{})
	return turn, true
}

// A local turn claim precedes the transport write. The prompt's sender alone
// closes this boundary; a session/cancel must not reach the peer ahead of it.
func (t *clientTurn) finishSending() { close(t.sending) }

func (t *clientTurn) complete() {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	t.running = false
	t.release()
}

// Called under the owner's lock. A pending permission handler retains its own
// turn record, but cannot keep a completed session in the connection's index.
func (t *clientTurn) release() {
	if !t.running && t.cancelling == 0 && t.owner.sessions[t.session] == t {
		delete(t.owner.sessions, t.session)
	}
}

func (t *clientTurns) session(id SessionID) *clientTurn {
	if t.sessions == nil {
		t.sessions = make(map[SessionID]*clientTurn)
	}
	turn := t.sessions[id]
	if turn == nil {
		turn = &clientTurn{owner: t, session: id, pending: make(map[*inboundRequest]struct{})}
		t.sessions[id] = turn
	}
	return turn
}

// Permission registration runs in wire order. The returned turn is also the
// cleanup target, so a late handler cannot alter the next turn's pending set.
func (t *clientTurns) registerPermission(session SessionID, call *inboundRequest) (permissionAdmission, *clientTurn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.sessions[session]
	if turn == nil || !turn.running {
		return permissionUnowned, nil
	}
	if turn.cancelled {
		return permissionCancelled, nil
	}
	turn.pending[call] = struct{}{}
	return permissionOwned, turn
}

func (t *clientTurn) forgetPermission(call *inboundRequest) {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	delete(t.pending, call)
}

// Every concurrent cancellation observes the same pending responses until they
// settle. Taking an exclusive snapshot would let a second cancellation overtake
// writes still owned by the first.
func (t *clientTurns) beginCancellation(session SessionID) clientCancellation {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.session(session)
	turn.cancelled = true
	turn.cancelling++
	pending := make([]*inboundRequest, 0, len(turn.pending))
	for call := range turn.pending {
		pending = append(pending, call)
	}
	return clientCancellation{turn: turn, pending: pending}
}

func (c clientCancellation) finish() {
	turn := c.turn
	turn.owner.mu.Lock()
	defer turn.owner.mu.Unlock()
	turn.cancelling--
	turn.release()
}

func (c clientCancellation) awaitPrompt(ctx context.Context) error {
	if c.turn.sending == nil {
		return nil
	}
	select {
	case <-c.turn.sending:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// An agentTurn is what an agent knows about one session's current turn.
type agentTurn struct {
	request *inboundRequest
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

func (t *agentTurns) claim(session SessionID, request *inboundRequest) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = make(map[SessionID]agentTurn)
	}
	if turn, running := t.sessions[session]; running && !turn.committed {
		return false
	}
	t.sessions[session] = agentTurn{request: request}
	return true
}

func (t *agentTurns) release(session SessionID, request *inboundRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if running && turn.request == request {
		delete(t.sessions, session)
	}
}

func (t *agentTurns) cancel(session SessionID) (*inboundRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if running && !turn.committed {
		turn.cancelled = true
		t.sessions[session] = turn
		return turn.request, true
	}
	return turn.request, false
}

// Deciding result-against-cancellation inside the aggregate is what makes it one
// decision: checking a flag and then writing outside would leave a window in
// which both a normal result and a cancellation appeared to win.
func (t *agentTurns) commit(session SessionID, request *inboundRequest) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if !running || turn.request != request {
		return false
	}
	turn.committed = true
	t.sessions[session] = turn
	return turn.cancelled
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
