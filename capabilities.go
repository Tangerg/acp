package acp

import "slices"

// The capability table: which methods a peer may be asked to serve, and on what
// condition.
//
// It is hand-maintained, and it has to be. The schema annotates 74 payloads with
// x-method and x-side, and has no annotation at all linking a method to the
// capability that gates it — those links exist only in prose descriptions, and
// some capabilities describe accepted data rather than an optional method.
// "Whatever the schema's capability types say" is not implementable.
//
// So the table is written out, with the schema's own words quoted beside each
// predicate, and capabilities_internals_test.go holds it against the generated
// method table in both directions: a method with no row, and a row naming a
// method the schema no longer has. A schema bump that adds or drops a method then
// fails loudly rather than leaving a hole in the gate.
//
// It is complete now rather than grown a layer at a time, because the layers need
// it in the wrong order: the connection exchanges capabilities and reserves every
// standard method name before any gated method is implemented. Later work
// activates rows; it does not invent the classification then.

// A gating says on what condition a standard method may be called.
type gating uint8

const (
	// gatingBaseline is a method every peer of its side implements. There is no
	// capability behind it, and refusing one would refuse the protocol.
	gatingBaseline gating = iota
	// gatingCapability is a method the serving peer must have advertised during
	// initialize. An unadvertised call is refused in both directions: outbound
	// because the peer never offered it, inbound because the caller was told it
	// was not there.
	gatingCapability
	// gatingUnimplemented is a method the specification defines and this package
	// does not serve yet. It is refused with method-not-found, which is the
	// honest answer, and it is a classification rather than an omission: the row
	// exists so that the table stays complete.
	//
	// Such a row still carries its predicate where one exists, because
	// construction reads it: advertising a capability whose method this package
	// cannot serve is a promise that will be broken on the first call, and the
	// place to refuse it is before a connection exists.
	gatingUnimplemented
)

// A methodGate is the authority check for one standard method.
type methodGate struct {
	gating gating
	// capability is the schema path the predicate reads, for the message a
	// refusal carries.
	capability string
	// owner is the peer whose capabilities the predicate reads, which is not
	// always the peer that serves the method. The mcp methods are the case that
	// makes this a field rather than an assumption: the client serves them, and
	// the agent's mcpCapabilities.acp is what says they may be used at all.
	owner methodSide
	// advertised reports whether the peer that serves this method said it would.
	// nil unless gating is gatingCapability.
	advertised func(PeerInfo) bool
	// why quotes what the schema says, so that a reader can check the
	// classification against the specification rather than against this comment.
	why string
}

// methodGates classifies every method the specification defines.
var methodGates = map[string]methodGate{
	// -- Baseline -------------------------------------------------------------
	//
	// The lifecycle, one turn, and cancellation. A peer that cannot serve these
	// cannot take part.
	methodInitialize: {
		gating: gatingBaseline,
		why:    "the capability exchange itself, so nothing can gate it",
	},
	methodAuthenticate: {
		gating: gatingBaseline,
		why: "no capability gates it; an agent asks for it by listing authMethods in its " +
			"initialize response, or by answering session/new with -32000",
	},
	methodSessionNew: {
		gating: gatingBaseline,
		why:    "every agent creates sessions; there would be nothing to gate",
	},
	methodSessionPrompt: {
		gating: gatingBaseline,
		why:    "the operation the protocol exists for",
	},
	methodSessionCancel: {
		gating: gatingBaseline,
		why:    "a baseline session operation with no capability behind it",
	},
	methodSessionUpdate: {
		gating: gatingBaseline,
		why: "the agent's running commentary for a turn, which a client must accept — including " +
			"after it has sent session/cancel",
	},
	methodSessionRequestPermission: {
		gating: gatingBaseline,
		why: "baseline client behaviour: the wire outcome is either cancelled or a selected " +
			"option, and an agent need not offer a reject option, so there is no universal " +
			"deny to synthesise for a client that will not answer",
	},
	methodSessionSetMode: {
		gating: gatingBaseline,
		why: "gated by data rather than by a capability: an agent offers modes by returning " +
			"them from session/new, and a client with no modes has nothing to set",
	},
	methodCancelRequest: {
		gating: gatingBaseline,
		why:    "the connection's own, and the only method that belongs to neither peer",
	},

	// -- Gated ----------------------------------------------------------------

	methodSessionLoad: {
		gating:     gatingCapability,
		capability: "agentCapabilities.loadSession",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return peer.AgentCapabilities.LoadSession },
		why:        `"Whether the agent supports session/load", and the request says "only available if" so`,
	},
	methodFsReadTextFile: {
		gating:     gatingCapability,
		capability: "clientCapabilities.fs.readTextFile",
		owner:      sideClient,
		advertised: func(peer PeerInfo) bool { return peer.ClientCapabilities.Fs.ReadTextFile },
		why:        `"Whether the Client supports fs/read_text_file requests"`,
	},
	methodFsWriteTextFile: {
		gating:     gatingCapability,
		capability: "clientCapabilities.fs.writeTextFile",
		owner:      sideClient,
		advertised: func(peer PeerInfo) bool { return peer.ClientCapabilities.Fs.WriteTextFile },
		why: `"Whether the Client supports fs/write_text_file requests" — a second boolean, so ` +
			"reading and writing are two capabilities and not one",
	},

	// One boolean for five methods. The schema says so in as many words —
	// "Whether the Client support all terminal/* methods" — which is why the
	// handler group for it is all five or none.
	methodTerminalCreate:      terminalGate,
	methodTerminalOutput:      terminalGate,
	methodTerminalWaitForExit: terminalGate,
	methodTerminalKill:        terminalGate,
	methodTerminalRelease:     terminalGate,

	// -- Not implemented yet --------------------------------------------------
	//
	// Each of these has a capability-to-handler grouping to work out before it can
	// be served, and the row is here so that the table is complete rather than as
	// far as the work has got.

	methodLogout: {
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.auth.logout",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return !peer.AgentCapabilities.Auth.Logout.IsZero() },
		why:        `"Whether the agent supports the logout method"`,
	},
	methodSessionList:   sessionCapability("list", func(s SessionCapabilities) bool { return !s.List.IsZero() }),
	methodSessionDelete: sessionCapability("delete", func(s SessionCapabilities) bool { return !s.Delete.IsZero() }),
	methodSessionFork:   sessionCapability("fork", func(s SessionCapabilities) bool { return !s.Fork.IsZero() }),
	methodSessionResume: sessionCapability("resume", func(s SessionCapabilities) bool { return !s.Resume.IsZero() }),
	methodSessionClose:  sessionCapability("close", func(s SessionCapabilities) bool { return !s.Close.IsZero() }),
	methodSessionSetConfigOption: {
		gating: gatingUnimplemented,
		why:    "gated by data, like session/set_mode: an agent offers config options by returning them",
	},

	methodProvidersList:    providersCapability,
	methodProvidersSet:     providersCapability,
	methodProvidersDisable: providersCapability,

	methodElicitationCreate: {
		gating:     gatingUnimplemented,
		capability: "clientCapabilities.elicitation",
		owner:      sideClient,
		advertised: func(peer PeerInfo) bool { return !peer.ClientCapabilities.Elicitation.IsZero() },
		why:        `"Determines which elicitation modes the agent may use"`,
	},
	methodElicitationComplete: {
		gating:     gatingUnimplemented,
		capability: "clientCapabilities.elicitation.url",
		owner:      sideClient,
		advertised: elicitationURL,
		why:        "the completion notification for a URL elicitation, so the url mode gates it",
	},

	methodMcpConnect:    mcpCapability,
	methodMcpMessage:    mcpCapability,
	methodMcpDisconnect: mcpCapability,

	methodNesStart:   agentNesCapability,
	methodNesSuggest: agentNesCapability,
	methodNesAccept:  agentNesCapability,
	methodNesReject:  agentNesCapability,
	methodNesClose:   agentNesCapability,

	methodDocumentDidOpen:   agentNesEvents,
	methodDocumentDidChange: agentNesEvents,
	methodDocumentDidClose:  agentNesEvents,
	methodDocumentDidSave:   agentNesEvents,
	methodDocumentDidFocus:  agentNesEvents,
}

// sessionCapability builds a row for one of the agent's session-lifecycle
// methods, each of which is gated by its own property of one capability object.
func sessionCapability(name string, set func(SessionCapabilities) bool) methodGate {
	return methodGate{
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.sessionCapabilities." + name,
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return set(peer.AgentCapabilities.SessionCapabilities) },
		why:        `"Whether the agent supports session/` + name + `"`,
	}
}

// The rows several methods share, because the capability does.
var (
	providersCapability = methodGate{
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.providers",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return !peer.AgentCapabilities.Providers.IsZero() },
		why:        "one capability object gating all three provider methods",
	}
	mcpCapability = methodGate{
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.mcpCapabilities.acp",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return peer.AgentCapabilities.McpCapabilities.Acp },
		why:        "the client holds the MCP connection on the agent's behalf, over the ACP channel",
	}
	agentNesCapability = methodGate{
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.nes",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return !peer.AgentCapabilities.Nes.IsZero() },
		why:        "one capability object gating all five next-edit methods",
	}
	agentNesEvents = methodGate{
		gating:     gatingUnimplemented,
		capability: "agentCapabilities.nes.events",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return !peer.AgentCapabilities.Nes.IsZero() },
		why:        "the document-sync notifications exist to feed the agent's next-edit events",
	}
)

func elicitationURL(peer PeerInfo) bool {
	elicitation, advertised := peer.ClientCapabilities.Elicitation.Get()
	return advertised && !elicitation.URL.IsZero()
}

// terminalGate is one row shared by five methods, because the capability is one
// boolean covering all five. Spelling it once is what keeps them from drifting
// apart.
var terminalGate = methodGate{
	gating:     gatingCapability,
	capability: "clientCapabilities.terminal",
	owner:      sideClient,
	advertised: func(peer PeerInfo) bool { return peer.ClientCapabilities.Terminal },
	why:        `"Whether the Client support all terminal/* methods" — one flag for five methods`,
}

// checkAdvertisement refuses an advertisement this side cannot serve.
//
// It is driven by the capability table rather than by a hand-written list of
// capability fields, so a capability nobody remembered to check is not a hole:
// every row that names a predicate is consulted, and a method this package does
// not implement at all cannot be advertised however its capability is spelled.
//
// The rule is about methods. A capability describing data an existing handler
// accepts — prompt content types, position encodings — is an explicit refinement
// and not a promise of a separate method, so it has no row here and is left to
// the caller.
func checkAdvertisement(peer PeerInfo, owner methodSide, implemented func(string) bool) []string {
	var exceeded []string
	for _, method := range sortedMethods() {
		gate := methodGates[method]
		if gate.advertised == nil || gate.owner != owner {
			continue
		}
		if !gate.advertised(peer) {
			continue
		}
		switch {
		case gate.gating == gatingUnimplemented:
			exceeded = append(exceeded, gate.capability+
				" advertises "+method+", which this package does not implement")
		case !implemented(method):
			exceeded = append(exceeded, gate.capability+
				" advertises "+method+" without the handler that serves it")
		}
	}
	return exceeded
}

// sortedMethods keeps a construction failure's message stable, so that two runs
// of the same misconfiguration report the same thing.
func sortedMethods() []string {
	methods := make([]string, 0, len(methodGates))
	for method := range methodGates {
		methods = append(methods, method)
	}
	slices.Sort(methods)
	return methods
}

// allowsMethod reports whether the peer that serves name advertised it, and names
// the capability when it did not.
//
// A method with no row is refused: the table covers every method the schema
// defines, so a name that is not in it is not a standard method, and an extension
// method does not come through here.
func allowsMethod(peer PeerInfo, name string) (allowed bool, capability string) {
	gate, known := methodGates[name]
	switch {
	case !known, gate.gating == gatingUnimplemented:
		return false, ""
	case gate.gating == gatingBaseline:
		return true, ""
	default:
		return gate.advertised(peer), gate.capability
	}
}
