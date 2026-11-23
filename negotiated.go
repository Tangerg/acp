package acp

import (
	"sync"
	"sync/atomic"
)

// Negotiated is what initialize settled between the two peers: both sides'
// capabilities, identities and _meta, and the authentication methods the agent
// offered. The same value backs the capability gate, so [ClientConn.Negotiated]
// and [AgentConn.Negotiated] hand out a copy all the way down.
type Negotiated struct {
	ClientCapabilities ClientCapabilities
	ClientInfo         Opt[Implementation]
	ClientMeta         Opt[Meta]

	AgentCapabilities AgentCapabilities
	AgentInfo         Opt[Implementation]
	AgentMeta         Opt[Meta]

	// AuthMethods is where [ClientConn.Authenticate] takes its identifier from.
	AuthMethods []AuthMethod
}

// negotiation owns a connection's agreement. The agreement is replaced whole and
// read without copying, because once stored it is never mutated.
type negotiation struct {
	agreement atomic.Pointer[Negotiated]
	// opened closes once this side may send: on the client when initialize is
	// accepted, on the agent once the first accepted answer is on the wire, so
	// nothing it sends can precede the response that told the client what the
	// connection carries.
	opened   chan struct{}
	openOnce sync.Once
}

func newNegotiation() *negotiation {
	return &negotiation{opened: make(chan struct{})}
}

// agreed returns the current agreement, or nil before initialize was accepted.
func (n *negotiation) agreed() *Negotiated { return n.agreement.Load() }

func (n *negotiation) accept(agreed Negotiated) {
	stored := deepCopy(agreed)
	n.agreement.Store(&stored)
}

func (n *negotiation) open() { n.openOnce.Do(func() { close(n.opened) }) }

// advertises reports whether the agreement permits a standard method, and the
// capability that decided it. A method with no gate row is not standard.
func (n *Negotiated) advertises(method string) (bool, string) {
	gate, known := gates[method]
	if !known {
		return false, "a standard method"
	}
	if gate == nil {
		return true, ""
	}
	return gate.advertisedBy(n), gate.path()
}

// refusesInbound answers a peer's request for a method this side never
// advertised: method-not-found, because that is what it was told.
func (n *Negotiated) refusesInbound(method string) *Error {
	advertised, capability := n.advertises(method)
	if advertised {
		return nil
	}
	return newError(ErrorCodeMethodNotFound, "method %q was not advertised because %s is not set", method, capability)
}

// refusesOutbound refuses locally a call the peer never advertised.
func (n *Negotiated) refusesOutbound(method string) error {
	advertised, capability := n.advertises(method)
	if advertised {
		return nil
	}
	return notAdvertised("method "+method, capability)
}

// authenticates checks an authenticate method identifier against what the agent
// offered. A terminal method is performed by running the agent again in a
// terminal, so the schema forbids passing it to authenticate.
func (n *Negotiated) authenticates(methodID AuthMethodID) error {
	return authenticationMethods(n.AuthMethods).accepts(methodID)
}
