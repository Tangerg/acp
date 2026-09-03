package acp

import (
	"maps"
	"slices"
)

// The capability table: which methods a peer may be asked to serve, and on what
// condition.
//
// It is hand-maintained, and it has to be. The schema annotates 46 payloads with
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
	//
	// No row uses it as of schema-v1.21.0; elicitation was the last one and is
	// served now. It stays because the table has to be able to say this. A method
	// arriving in a schema bump has to be classified — the test that holds this
	// table against the generated method list sees to that — and the only
	// alternatives to saying "known, not served" would be to claim it is baseline
	// or to claim a capability gates something nothing implements. Both are
	// lies the gate would then act on.
	gatingUnimplemented
)

// A methodGate is the authority check for one standard method.
type methodGate struct {
	gating gating
	// capability is the schema path the predicate reads, for the message a
	// refusal carries.
	capability string
	// owner is the peer whose capabilities the predicate reads.
	owner methodSide
	// advertised reports whether the peer that serves this method said it would.
	// nil unless gating is gatingCapability.
	advertised func(PeerInfo) bool
	// why quotes what the schema says, so that a reader can check the
	// classification against the specification rather than against this comment.
	why string
}

// A gateTable classifies every method the specification defines. It answers two
// questions, and both are its own: whether a negotiated peer permits a method,
// and what an advertisement promises that its side cannot serve.
type gateTable map[string]methodGate

// gates classifies every method the specification defines.
var gates = gateTable{
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

	methodLogout: {
		gating:     gatingCapability,
		capability: "agentCapabilities.auth.logout",
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return hasCapability(peer.AgentCapabilities.Auth.Logout) },
		why:        `"Whether the agent supports the logout method"`,
	},
	methodSessionList:   sessionCapability("list", func(s SessionCapabilities) bool { return hasCapability(s.List) }),
	methodSessionDelete: sessionCapability("delete", func(s SessionCapabilities) bool { return hasCapability(s.Delete) }),
	methodSessionResume: sessionCapability("resume", func(s SessionCapabilities) bool { return hasCapability(s.Resume) }),
	methodSessionClose:  sessionCapability("close", func(s SessionCapabilities) bool { return hasCapability(s.Close) }),
	methodSessionSetConfigOption: {
		gating: gatingBaseline,
		why: "gated by data rather than by a capability, like session/set_mode: an agent offers " +
			"config options by returning them, and an agent that offers none has nothing to set",
	},

	// Elicitation is one group and not two methods: a request carries a mode and a
	// scope as two flattened unions, url mode answers asynchronously through
	// elicitation/complete under an identifier of its own, and form mode hands the
	// client a JSON Schema to render.
	//
	// The method capability is `elicitation` alone. Which of the two modes a client
	// can render is `elicitation.form` and `elicitation.url`, and those gate a
	// parameter rather than a method — see [PeerInfo.permitsElicitationMode], and
	// the paragraph below on why the two kinds are enforced in different
	// directions. The exception is elicitation/complete, which exists only to
	// finish a url elicitation, so the url mode gates the method itself.
	methodElicitationCreate: {
		gating:     gatingCapability,
		capability: "clientCapabilities.elicitation",
		owner:      sideClient,
		advertised: func(peer PeerInfo) bool { return hasCapability(peer.ClientCapabilities.Elicitation) },
		why:        `"Determines which elicitation modes the agent may use"`,
	},
	methodElicitationComplete: {
		gating:     gatingCapability,
		capability: "clientCapabilities.elicitation.url",
		owner:      sideClient,
		advertised: elicitationURL,
		why:        "the completion notification for a URL elicitation, so the url mode gates it",
	},
}

func sessionCapability(name string, set func(SessionCapabilities) bool) methodGate {
	return methodGate{
		gating:     gatingCapability,
		capability: "agentCapabilities.sessionCapabilities." + name,
		owner:      sideAgent,
		advertised: func(peer PeerInfo) bool { return set(peer.AgentCapabilities.SessionCapabilities) },
		why:        `"Whether the agent supports session/` + name + `"`,
	}
}

func elicitationURL(peer PeerInfo) bool {
	elicitation, advertised := peer.ClientCapabilities.Elicitation.Get()
	return advertised && hasCapability(elicitation.URL)
}

// Capability objects use present `{}` as true and both absent and null as
// false. IsZero cannot answer that question because null is intentionally a
// non-zero Opt state.
func hasCapability[T any](capability Opt[T]) bool {
	_, present := capability.Get()
	return present
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

// It is driven by the table rather than by a hand-written list of capability
// fields, so a capability nobody remembered to check is not a hole: every row that
// names a predicate is consulted, and a method this package does not implement at
// all cannot be advertised however its capability is spelled.
//
// The rule is about methods. A capability describing data an existing handler
// accepts — such as prompt content types — is an explicit refinement
// and not a promise of a separate method, so it has no row here and is left to the
// caller.
//
// The methods are visited in order so that two runs of the same misconfiguration
// report the same thing.
func (t gateTable) exceeded(peer PeerInfo, owner methodSide, implemented func(string) bool) []string {
	var exceeded []string
	for _, method := range slices.Sorted(maps.Keys(t)) {
		gate := t[method]
		if gate.advertised == nil || gate.owner != owner || !gate.advertised(peer) {
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
