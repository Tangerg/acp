package acp

import (
	"context"
	"sync"
)

// A turn is one session/prompt. session/cancel names a session and not a turn,
// so a session has at most one active turn, and each side keeps that rule and
// the obligations around cancelling it in one piece of per-session state with
// one lock.

// clientTurns owns, per session, the active turn and the cancellations writing
// for it. A turn record is its identity: delayed work keeps the old record rather
// than finding a newer turn by its session ID.
type clientTurns struct {
	mu       sync.Mutex
	sessions map[SessionID]*clientTurn
}

type clientTurn struct {
	owner   *clientTurns
	session SessionID
	running bool
	// sent closes once the prompt's write has finished, which a cancellation
	// waits for so that it cannot reach the peer ahead of the prompt it names.
	sent chan struct{}
	// cancelled is set when a cancellation is committed to the wire. Permission
	// requests that arrive afterwards are answered cancelled without a dialog.
	cancelled bool
	// cancelling counts cancellations still writing; idle closes when it returns
	// to zero, so a prompt started meanwhile waits rather than being cancelled.
	cancelling int
	idle       chan struct{}
	pending    map[*inboundRequest]struct{}
}

// clientCancellation is one in-flight session/cancel and the permission requests
// it owes a cancelled answer.
type clientCancellation struct {
	turn    *clientTurn
	pending []*inboundRequest
}

// begin claims the session's turn, waiting out a cancellation that is still
// writing.
func (t *clientTurns) begin(ctx context.Context, session SessionID) (*clientTurn, error) {
	for {
		t.mu.Lock()
		turn := t.turn(session)
		switch {
		case turn.running:
			t.mu.Unlock()
			return nil, ErrPromptInProgress
		case turn.cancelling > 0:
			idle := turn.idle
			t.mu.Unlock()
			select {
			case <-idle:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
			turn.running = true
			turn.cancelled = false
			turn.sent = make(chan struct{})
			t.mu.Unlock()
			return turn, nil
		}
	}
}

// finishSending closes the prompt's send boundary. Only the prompt's sender
// calls it.
func (t *clientTurn) finishSending() { close(t.sent) }

func (t *clientTurn) complete() {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	t.running = false
	t.release()
}

// release forgets a turn record nothing refers to any more. Called under the
// owner's lock.
func (t *clientTurn) release() {
	if !t.running && t.cancelling == 0 && t.owner.sessions[t.session] == t {
		delete(t.owner.sessions, t.session)
	}
}

func (t *clientTurns) turn(id SessionID) *clientTurn {
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

// A permissionAdmission is how a permission request relates to its session's
// turn, decided in wire order.
type permissionAdmission uint8

const (
	// permissionUnowned has no active turn: the grammar does not make permission
	// requests children of a prompt, so it is served normally.
	permissionUnowned permissionAdmission = iota
	permissionOwned
	permissionCancelled
)

// registerPermission files a permission request under its session's turn. The
// returned turn is also its cleanup target, so a late handler cannot alter the
// next turn's pending set.
func (t *clientTurns) registerPermission(session SessionID, call *inboundRequest) (permissionAdmission, *clientTurn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.sessions[session]
	switch {
	case turn == nil || !turn.running:
		return permissionUnowned, nil
	case turn.cancelled:
		return permissionCancelled, nil
	default:
		turn.pending[call] = struct{}{}
		return permissionOwned, turn
	}
}

func (t *clientTurn) forgetPermission(call *inboundRequest) {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	delete(t.pending, call)
}

// beginCancellation commits a cancellation. Every concurrent cancellation sees
// the same pending requests until they settle, so one cannot overtake writes the
// other still owns.
func (t *clientTurns) beginCancellation(session SessionID) clientCancellation {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.turn(session)
	turn.cancelled = true
	if turn.cancelling == 0 {
		turn.idle = make(chan struct{})
	}
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
	if turn.cancelling == 0 {
		close(turn.idle)
	}
	turn.release()
}

// promptSent is closed once the cancelled turn's prompt has been written, or
// immediately when no prompt is running.
func (c clientCancellation) promptSent() <-chan struct{} {
	c.turn.owner.mu.Lock()
	defer c.turn.owner.mu.Unlock()
	if c.turn.sent == nil {
		sent := make(chan struct{})
		close(sent)
		return sent
	}
	return c.turn.sent
}

// An agentTurn is what an agent knows about one session's current turn.
type agentTurn struct {
	request *inboundRequest
	// cancelled makes session/cancel a semantic outcome; the request context
	// alone cannot tell it from connection shutdown.
	cancelled bool
	// committed means the result is decided. A cancellation after it is too
	// late to change it, even while the response is still being written.
	committed bool
}

// agentTurns owns the request session/cancel targets, so connection code never
// observes a half-updated turn.
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
	if turn, running := t.sessions[session]; running && turn.request == request {
		delete(t.sessions, session)
	}
}

// cancel marks the session's uncommitted turn cancelled and returns its request.
func (t *agentTurns) cancel(session SessionID) (*inboundRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn, running := t.sessions[session]
	if !running || turn.committed {
		return nil, false
	}
	turn.cancelled = true
	t.sessions[session] = turn
	return turn.request, true
}

// commit decides result against cancellation inside the aggregate, so that a
// normal result and a cancellation cannot both appear to win. It reports whether
// the turn was cancelled first.
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
