package acp

import (
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

	peerMu      sync.Mutex
	peer        PeerInfo
	initialized bool
}

// Peer reports what initialize negotiated. Before initialize it is the zero value.
//
// It is a copy, all the way down. The same value backs the capability gate, and a
// caller who could mutate it could widen its own authority.
func (c *connection) Peer() PeerInfo {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	return c.peer.clone()
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

func (c *connection) negotiated(peer PeerInfo) {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	c.peer = peer
	c.initialized = true
}

func (c *connection) isInitialized() bool {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	return c.initialized
}

// sessions is one handle per identifier per connection.
//
// One rather than one per lookup, because the rules a handle keeps are the
// session's: two handles for one session would each believe they were the only
// turn, and the one-prompt-at-a-time rule would hold for neither.
type sessions[Handle any] struct {
	mu   sync.Mutex
	byID map[SessionID]*Handle
}

func (s *sessions[Handle]) lookup(id SessionID, open func(SessionID) *Handle) *Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = make(map[SessionID]*Handle)
	}
	if existing, ok := s.byID[id]; ok {
		return existing
	}
	handle := open(id)
	s.byID[id] = handle
	return handle
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
