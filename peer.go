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

// A method with no row is refused: the table covers every method the schema
// defines, so a name that is not in it is not a standard method, and an extension
// method does not come through here.
//
// The refusal is method-not-found rather than a code of its own, because from the
// caller's side that is the truth: it was told during initialize that this method
// was not there. The message names the capability so that a developer reading a
// log can see which advertisement was missing.
func (p PeerInfo) permits(method string) error {
	gate, known := gates[method]
	if !known || gate.gating == gatingUnimplemented {
		return methodNotImplemented(method)
	}
	if gate.gating == gatingBaseline || gate.advertised(p) {
		return nil
	}
	return newError(ErrorCodeMethodNotFound,
		"method %q was not advertised because %s is not set", method, gate.capability)
}

// permitsSessionSetup checks the two gated parameters that session/new,
// session/load and session/resume all carry.
func (p PeerInfo) permitsSessionSetup(servers []McpServer, directories []string) error {
	mcp := p.AgentCapabilities.McpCapabilities
	for _, server := range servers {
		transport, advertised, capability := "", true, ""
		switch server.(type) {
		case *McpServerHTTP:
			transport, advertised, capability =
				"http", mcp.HTTP, "agentCapabilities.mcpCapabilities.http"
		case *McpServerSse:
			transport, advertised, capability =
				"sse", mcp.Sse, "agentCapabilities.mcpCapabilities.sse"
		}
		if !advertised {
			return newError(ErrorCodeInvalidParams,
				"a %q MCP server was not advertised because %s is not set", transport, capability)
		}
	}
	if len(directories) > 0 &&
		!hasCapability(p.AgentCapabilities.SessionCapabilities.AdditionalDirectories) {
		return newError(ErrorCodeInvalidParams,
			"additionalDirectories was not advertised because "+
				"agentCapabilities.sessionCapabilities.additionalDirectories is not set")
	}
	return nil
}

// The protocol gates three request parameters as well as the methods that carry
// them, and the specification puts all three MUSTs on the client: it must
// restrict prompt content to the advertised prompt capabilities, must verify the
// agent's MCP capabilities before naming an http or sse server, and must only
// send additionalDirectories to an agent that advertised them.
//
// These are a different kind of capability from the ones [PeerInfo.permits]
// enforces, and the difference decides who checks. A method capability is
// authority — whether an agent may read a file or run a command — so it is
// enforced in both directions and a peer that ignores it is refused at the door.
// A parameter capability is comprehension: an agent that did not advertise images
// is saying it cannot read one, not that it forbids being sent one. So it is
// checked where the specification puts the obligation, on the way out, and an
// agent built on this package does not refuse work it can actually do.
//
// The refusal is invalid-params rather than method-not-found: the method is
// available, and it is what a caller put inside it that the agent never said it
// could take.
func (p PeerInfo) permitsPromptContent(blocks []ContentBlock) error {
	prompt := p.AgentCapabilities.PromptCapabilities
	for _, block := range blocks {
		kind, advertised, capability := "", true, ""
		switch block.(type) {
		case *ImageContent:
			kind, advertised, capability =
				"image", prompt.Image, "agentCapabilities.promptCapabilities.image"
		case *AudioContent:
			kind, advertised, capability =
				"audio", prompt.Audio, "agentCapabilities.promptCapabilities.audio"
		case *EmbeddedResource:
			kind, advertised, capability =
				"resource", prompt.EmbeddedContext, "agentCapabilities.promptCapabilities.embeddedContext"
		}
		if !advertised {
			return newError(ErrorCodeInvalidParams,
				"a %q content block was not advertised because %s is not set", kind, capability)
		}
	}
	return nil
}

// permitsConfigOptionValue is the fourth parameter capability, and the only one a
// side reads about itself.
//
// The schema grants the boolean option type through the client rather than the
// agent: supplying `session.configOptions.boolean` "means agents may include
// `type: "boolean"` entries in `configOptions`, and the client may send
// `session/set_config_option` requests with `type: "boolean"` and a boolean
// `value`". So the permission to send one is the client's own advertisement, and
// this is the client checking what it said.
//
// That is worth checking rather than assuming. A client sending a boolean value
// for an option type it told the agent it does not support is describing an option
// the agent could not have offered, and the mistake is easier to read here than in
// whatever the agent answers.
//
// The select type has no capability. The schema gates only the boolean one, which
// is what a later addition to an already-shipped grammar looks like.
func (p PeerInfo) permitsConfigOptionValue(value SetSessionConfigOptionRequestValue) error {
	if _, boolean := value.(*SetSessionConfigOptionRequestBoolean); !boolean {
		return nil
	}
	session, hasSession := p.ClientCapabilities.Session.Get()
	if hasSession {
		if options, hasOptions := session.ConfigOptions.Get(); hasOptions &&
			hasCapability(options.Boolean) {
			return nil
		}
	}
	return newError(ErrorCodeInvalidParams,
		"a boolean session configuration option was not advertised because "+
			"clientCapabilities.session.configOptions.boolean is not set")
}

// A mode is a parameter capability and not a method one: `elicitation` says the
// client serves the method, and the modes under it say which shapes it can render,
// so a mode it did not advertise is work it cannot do rather than authority it
// withheld. See [PeerInfo.permitsPromptContent] for why that decides the direction.
func (p PeerInfo) permitsElicitationMode(mode CreateElicitationRequestValue) error {
	elicitation, advertised := p.ClientCapabilities.Elicitation.Get()
	if !advertised {
		// Unreachable: the method gate refuses this first, and reaching it would
		// mean the two disagree.
		return unadvertisedMode("", "clientCapabilities.elicitation")
	}
	switch mode.(type) {
	case *ElicitationFormMode:
		if !hasCapability(elicitation.Form) {
			return unadvertisedMode(elicitationModeForm, "clientCapabilities.elicitation.form")
		}
	case *ElicitationURLMode:
		if !hasCapability(elicitation.URL) {
			return unadvertisedMode(elicitationModeURL, "clientCapabilities.elicitation.url")
		}
	}
	return nil
}
