package acp

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"
)

// A connection is what both ends of a link have: the link itself, and the peer
// the handshake settled on.
//
// Embedding it rather than repeating it is what stops a session reaching through
// two objects to make a call. It also puts the initialization phase in one place:
// the read loop is running before the handshake completes on either side, because
// on one side the answer arrives on it and on the other the question does, and
// anything else that arrives first would reach application code before there was a
// negotiated peer to judge it against.
type connection struct {
	*link
	handshake *handshake
}

func newConnection() connection {
	return connection{handshake: newHandshake()}
}

// Peer reports what initialize negotiated. Before initialize it is the zero value.
//
// It is a copy, all the way down. The same value backs the capability gate, and a
// caller who could mutate it could widen its own authority.
func (c *connection) Peer() PeerInfo {
	return c.handshake.Peer()
}

// Close ends the connection. It is idempotent and safe to call concurrently, and
// it reports the connection's terminal error — the same value [connection.Wait]
// reports.
func (c *connection) Close() error { return c.close() }

// Wait blocks until the connection has ended and everything it owns has stopped,
// then reports its terminal error: nil for a local close that released everything
// and for a clean end of stream, and otherwise the first read, write or release
// failure.
func (c *connection) Wait() error { return c.wait() }

// tooManySessions ends a connection whose peer has named more sessions than this
// side will hold. The handle already minted is honoured so that whatever is
// mid-flight fails on the connection ending rather than on a nil pointer.
func (c *connection) tooManySessions() {
	c.endReading(fmt.Errorf("%w: more than %d", errTooManySessions, maxSessionsPerConnection))
}

// handshake owns the transition from an unopened connection to an accepted and
// then published capability agreement. Acceptance and publication are separate
// because an agent must validate and retain initialize before encoding its
// response, but must not send anything else until that response is on the wire.
type handshake struct {
	mu         sync.Mutex
	state      handshakeState
	peer       PeerInfo
	active     *handshakeAttempt
	acceptedBy *handshakeAttempt
	pending    []*handshakeAttempt

	accepted  chan struct{}
	published chan struct{}
}

// handshakeAttempt serializes initialize calls without blocking the ordered
// delivery loop. Full-duplex transports cannot prove whether a call observed the
// preceding response merely from local goroutine scheduling, so later attempts
// wait for that response to settle before they are admitted or refused.
type handshakeAttempt struct {
	owner   *handshake
	ready   chan struct{}
	refused bool
}

type handshakeState uint8

const (
	handshakeIdle handshakeState = iota
	handshakeNegotiating
	handshakeAccepted
	handshakePublished
)

func newHandshake() *handshake {
	return &handshake{
		accepted:  make(chan struct{}),
		published: make(chan struct{}),
	}
}

// begin makes renegotiation unrepresentable while capabilities may be gating
// concurrent work.
func (h *handshake) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != handshakeIdle {
		return false
	}
	h.state = handshakeNegotiating
	return true
}

func (h *handshake) registerAttempt() *handshakeAttempt {
	h.mu.Lock()
	defer h.mu.Unlock()

	attempt := &handshakeAttempt{owner: h, ready: make(chan struct{})}
	switch h.state {
	case handshakeIdle:
		h.state = handshakeNegotiating
		h.active = attempt
		close(attempt.ready)
	case handshakeNegotiating:
		h.pending = append(h.pending, attempt)
	case handshakeAccepted, handshakePublished:
		attempt.refused = true
		close(attempt.ready)
	}
	return attempt
}

func (a *handshakeAttempt) await(ctx context.Context) error {
	select {
	case <-a.ready:
		if a.refused {
			return newError(ErrorCodeInvalidRequest,
				"this connection is already initializing or initialized")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// settle advances exactly the active attempt. A failed attempt promotes its
// ordered successor only after the failed answer is complete; acceptance instead
// closes the capability agreement and refuses every queued renegotiation.
func (a *handshakeAttempt) settle() {
	h := a.owner
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.active != a {
		for i, pending := range h.pending {
			if pending == a {
				h.pending = slices.Delete(h.pending, i, i+1)
				return
			}
		}
		return
	}

	h.active = nil
	if h.state != handshakeNegotiating {
		for _, pending := range h.pending {
			pending.refused = true
			close(pending.ready)
		}
		h.pending = nil
		return
	}

	h.state = handshakeIdle
	if len(h.pending) == 0 {
		return
	}
	next := h.pending[0]
	h.pending = h.pending[1:]
	h.state = handshakeNegotiating
	h.active = next
	close(next.ready)
}

// publish is attempt-bound because a failed answer may promote a successor that
// accepts before the failed request's goroutine finishes. Letting the old answer
// publish the new agreement would open outbound work ahead of its response.
func (a *handshakeAttempt) publish() {
	h := a.owner
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != handshakeAccepted || h.acceptedBy != a {
		return
	}
	h.state = handshakePublished
	close(h.published)
}

func (h *handshake) accept(peer PeerInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != handshakeNegotiating {
		panic("acp: handshake accepted outside negotiation")
	}
	h.peer = peer.clone()
	h.state = handshakeAccepted
	h.acceptedBy = h.active
	close(h.accepted)
}

func (h *handshake) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != handshakeAccepted || h.acceptedBy != nil {
		return
	}
	h.state = handshakePublished
	close(h.published)
}

func (h *handshake) Peer() PeerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peer.clone()
}

func (h *handshake) isAccepted() bool {
	select {
	case <-h.accepted:
		return true
	default:
		return false
	}
}

func (h *handshake) whenPublished() <-chan struct{} { return h.published }

// sessions is one handle per identifier per connection.
//
// One rather than one per lookup, because the rules a handle keeps are the
// session's: two handles for one session would each believe they were the only
// turn, and the one-prompt-at-a-time rule would hold for neither. That is also
// why a handle is never evicted, and why the population needs a bound instead:
// see limits.go.
type sessions[Handle any] struct {
	mu   sync.Mutex
	byID map[SessionID]*Handle
}

// lookup returns the handle for an identifier and whether this connection is
// still within its bound. The handle is real either way — the caller is mid-way
// through serving something and needs one — so a false only says the connection
// must now end.
func (s *sessions[Handle]) lookup(id SessionID, open func(SessionID) *Handle) (*Handle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = make(map[SessionID]*Handle)
	}
	if existing, ok := s.byID[id]; ok {
		return existing, true
	}
	handle := open(id)
	if len(s.byID) >= maxSessionsPerConnection {
		return handle, false
	}
	s.byID[id] = handle
	return handle, true
}

// registry lists the connections a client or agent has opened and not closed.
//
// It forgets an ended connection when it is next asked rather than when it ends,
// because nothing else has to be told: a connection that will accept nothing
// further is not open, however much is still being handed to the callers that were
// already waiting.
type registry[Conn interface{ ended() bool }] struct {
	mu   sync.Mutex
	open []Conn
}

func (r *registry[Conn]) add(connection Conn) {
	r.mu.Lock()
	r.open = append(r.open, connection)
	r.mu.Unlock()
}

func (r *registry[Conn]) all() iter.Seq[Conn] {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open = slices.DeleteFunc(r.open, func(connection Conn) bool { return connection.ended() })
	return slices.Values(slices.Clone(r.open))
}
