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
	// Always [CurrentProtocolVersion]: a protocol number names a grammar rather
	// than a feature level, so this package speaks the one it implements or
	// refuses the connection.
	ProtocolVersion ProtocolVersion

	ClientCapabilities ClientCapabilities
	ClientInfo         Opt[Implementation]

	AgentCapabilities AgentCapabilities
	AgentInfo         Opt[Implementation]

	// Where [ClientConn.Authenticate] takes its identifier from. Without it a
	// client cannot discover a method id and can only guess one it knew out of band.
	AuthMethods []AuthMethod

	// The one place an extension can say something about the connection itself, so
	// a snapshot that dropped them would lose it.
	ClientMeta Opt[Meta]
	AgentMeta  Opt[Meta]
}

// The struct copy is not enough, and neither is a shallow clone of the slices in
// it. Capabilities nest more than twenty reserved _meta maps inside each other,
// the auth methods are a slice of interfaces holding pointers, and a caller who
// could reach any of them would be holding the same memory the connection reads.
// See clone.go for why this is one reflective copy rather than a method per type.
func (p PeerInfo) clone() PeerInfo {
	return deepCopy(p)
}

// The schema says the identifier "must be one of the methods advertised in the
// initialize response", and that a client "MUST NOT pass" a terminal method to
// authenticate: that one is performed by running the agent again in an
// interactive terminal, so there is no call to make. An unadvertised identifier
// is the client guessing, which is the thing AuthMethods exists to stop.
func (p PeerInfo) authenticates(methodID AuthMethodID) error {
	return authenticationMethods(p.AuthMethods).accepts(methodID)
}
