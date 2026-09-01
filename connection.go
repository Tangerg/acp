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

// Peer reports what initialize negotiated, and the zero value before it has.
//
// It is a copy all the way down: the same value backs the capability gate, so a
// caller able to mutate it could widen its own authority.
func (c *connection) Peer() PeerInfo {
	return c.handshake.Peer()
}

// Close reports the connection's terminal error rather than a close error: they
// are the same value [connection.Wait] reports, and releasing what the connection
// owns is part of what can fail. It is idempotent.
func (c *connection) Close() error { return c.close() }

// Wait blocks until the connection has ended and everything it owns has stopped.
// It reports nil for a local close that released everything and for a peer that
// hung up cleanly, and otherwise the first read, write or release failure. Every
// caller sees the same value.
func (c *connection) Wait() error { return c.wait() }

// The handle already minted is still honoured, so that whatever is mid-flight
// fails on the connection ending rather than on a nil pointer.
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

// begin makes renegotiation unrepresentable while capabilities are already gating
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
// One rather than one per lookup because a handle is a caller-visible identity:
// two for one session would be two objects a caller could not tell apart. The
// turn invariant does not depend on it — that lives on the connection, keyed by
// identifier — so a handle can be forgotten once the protocol says the session is
// gone, which is the only thing that keeps the population from being one-way. See
// limits.go for the bound that covers everything else.
type sessions[Handle any] struct {
	mu   sync.Mutex
	byID map[SessionID]*Handle
}

// The handle is real whether or not the bound was breached: the caller is mid-way
// through serving something and needs one, so a false only says the connection
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

// forget is the only way this population shrinks; see limits.go.
func (s *sessions[Handle]) forget(id SessionID) {
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
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
