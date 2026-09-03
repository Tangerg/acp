package acp

import (
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
