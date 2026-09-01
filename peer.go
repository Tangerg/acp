package acp

// PeerInfo is a snapshot of what initialize negotiated.
//
// Capabilities are an authority boundary rather than presentation metadata: they
// decide whether an agent may read a file or run a command. So this is handed out
// as a copy, all the way down. The same value backs the capability gate, and a
// caller who could mutate it could widen its own authority.
//
// Both halves are here whichever side holds it. A client needs the agent's
// capabilities to know what it may call, and its own to know what it promised;
// the agent needs exactly the same two facts from the other direction.
type PeerInfo struct {
	// ProtocolVersion is the version initialize settled on, which is always
	// [CurrentProtocolVersion]: a protocol number names a grammar rather than a
	// feature level, so this package speaks the one it implements or refuses the
	// connection. See [CurrentProtocolVersion].
	ProtocolVersion ProtocolVersion

	// ClientCapabilities is what the client advertised. It gates the methods an
	// agent may call.
	ClientCapabilities ClientCapabilities
	// ClientInfo identifies the client, when it chose to say.
	ClientInfo Opt[Implementation]

	// AgentCapabilities is what the agent advertised. It gates the methods a
	// client may call.
	AgentCapabilities AgentCapabilities
	// AgentInfo identifies the agent, when it chose to say.
	AgentInfo Opt[Implementation]

	// AuthMethods is what the agent will accept from a client that must
	// authenticate, and is what [ClientConn.Authenticate] takes an identifier
	// from. Without it a client cannot discover a method id and can only guess one
	// it knew out of band.
	AuthMethods []AuthMethod

	// ClientMeta and AgentMeta are the _meta the two peers attached to their
	// halves of the handshake. The protocol reserves them for exactly this, and a
	// snapshot that dropped them would lose the one place an extension can say
	// something about the connection itself.
	ClientMeta Opt[Meta]
	AgentMeta  Opt[Meta]
}

// clone returns a copy that shares nothing this package defined.
//
// The struct copy is not enough, and neither is a shallow clone of the slices in
// it. Capabilities nest more than twenty reserved _meta maps inside each other,
// the auth methods are a slice of interfaces holding pointers, and a caller who
// could reach any of them would be holding the same memory the connection reads.
// See clone.go for why this is one reflective copy rather than a method per type.
func (p PeerInfo) clone() PeerInfo {
	return deepCopy(p)
}

// authenticates reports why methodID may not be passed to authenticate, or nil.
//
// The schema says the identifier "must be one of the methods advertised in the
// initialize response", and that a client "MUST NOT pass" a terminal method to
// authenticate: that one is performed by running the agent again in an
// interactive terminal, so there is no call to make. An unadvertised identifier
// is the client guessing, which is the thing AuthMethods exists to stop.
func (p PeerInfo) authenticates(methodID AuthMethodID) error {
	return authenticationMethods(p.AuthMethods).accepts(methodID)
}
