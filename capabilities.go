package acp

import (
	"maps"
	"slices"
)

// The capability table: on what condition a peer may be asked to serve each
// standard method.
//
// It is hand-maintained because the schema links a method to its gating
// capability only in prose. capabilities_internals_test.go holds it against the
// generated method table in both directions, so a schema release that adds or
// drops a method fails rather than leaving a hole in the gate.

// A methodCapability is the advertisement that gates a method: one leaf of one
// side's capabilities, read and written through the same descriptor so that the
// gate and the derived advertisement cannot name different leaves.
type methodCapability interface {
	path() string
	advertisedBy(agreed *Negotiated) bool
}

// An agentCapability is a leaf of [AgentCapabilities].
type agentCapability struct {
	leaf string
	get  func(AgentCapabilities) bool
	set  func(*AgentCapabilities)
}

func (c agentCapability) path() string { return "agentCapabilities." + c.leaf }

func (c agentCapability) advertisedBy(n *Negotiated) bool { return c.get(n.AgentCapabilities) }

// A clientCapability is a leaf of [ClientCapabilities]. set is nil where the
// advertisement is derived from a handler group rather than from one handler.
type clientCapability struct {
	leaf string
	get  func(ClientCapabilities) bool
	set  func(*ClientCapabilities)
}

func (c clientCapability) path() string { return "clientCapabilities." + c.leaf }

func (c clientCapability) advertisedBy(n *Negotiated) bool { return c.get(n.ClientCapabilities) }

// gates maps every standard method to its gating capability. A nil entry is a
// baseline method no capability gates: the lifecycle, a turn, cancellation, and
// the two session methods an agent offers by data rather than by advertisement
// (modes and configuration options it returns from session/new).
var gates = map[string]methodCapability{
	methodInitialize:               nil,
	methodAuthenticate:             nil,
	methodSessionNew:               nil,
	methodSessionPrompt:            nil,
	methodSessionCancel:            nil,
	methodSessionUpdate:            nil,
	methodSessionRequestPermission: nil,
	methodSessionSetMode:           nil,
	methodSessionSetConfigOption:   nil,
	methodCancelRequest:            nil,

	methodSessionLoad: agentCapability{
		leaf: "loadSession",
		get:  func(c AgentCapabilities) bool { return c.LoadSession },
		set:  func(c *AgentCapabilities) { c.LoadSession = true },
	},
	methodLogout: agentCapability{
		leaf: "auth.logout",
		get:  func(c AgentCapabilities) bool { return hasCapability(c.Auth.Logout) },
		set:  func(c *AgentCapabilities) { c.Auth.Logout = OptValue(LogoutCapabilities{}) },
	},
	methodSessionList: agentCapability{
		leaf: "sessionCapabilities.list",
		get:  func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.List) },
		set:  func(c *AgentCapabilities) { c.SessionCapabilities.List = OptValue(SessionListCapabilities{}) },
	},
	methodSessionDelete: agentCapability{
		leaf: "sessionCapabilities.delete",
		get:  func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Delete) },
		set:  func(c *AgentCapabilities) { c.SessionCapabilities.Delete = OptValue(SessionDeleteCapabilities{}) },
	},
	methodSessionResume: agentCapability{
		leaf: "sessionCapabilities.resume",
		get:  func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Resume) },
		set:  func(c *AgentCapabilities) { c.SessionCapabilities.Resume = OptValue(SessionResumeCapabilities{}) },
	},
	methodSessionClose: agentCapability{
		leaf: "sessionCapabilities.close",
		get:  func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Close) },
		set:  func(c *AgentCapabilities) { c.SessionCapabilities.Close = OptValue(SessionCloseCapabilities{}) },
	},

	methodFsReadTextFile: clientCapability{
		leaf: "fs.readTextFile",
		get:  func(c ClientCapabilities) bool { return c.Fs.ReadTextFile },
		set:  func(c *ClientCapabilities) { c.Fs.ReadTextFile = true },
	},
	methodFsWriteTextFile: clientCapability{
		leaf: "fs.writeTextFile",
		get:  func(c ClientCapabilities) bool { return c.Fs.WriteTextFile },
		set:  func(c *ClientCapabilities) { c.Fs.WriteTextFile = true },
	},
	// One boolean for all five terminal methods, which is why the handler group
	// is all five or none.
	methodTerminalCreate:      terminalCapability,
	methodTerminalOutput:      terminalCapability,
	methodTerminalWaitForExit: terminalCapability,
	methodTerminalKill:        terminalCapability,
	methodTerminalRelease:     terminalCapability,

	// The elicitation modes gate a parameter rather than a method; the method is
	// available when any mode is. elicitation/complete exists only to finish a
	// URL elicitation, so the url mode gates it.
	methodElicitationCreate: clientCapability{
		leaf: "elicitation",
		get:  anyElicitationMode,
	},
	methodElicitationComplete: clientCapability{
		leaf: elicitationURL.leaf,
		get:  elicitationURL.advertisedIn,
	},
}

var terminalCapability = clientCapability{
	leaf: "terminal",
	get:  func(c ClientCapabilities) bool { return c.Terminal },
	set:  func(c *ClientCapabilities) { c.Terminal = true },
}

// hasCapability reads a capability object: a present `{}` is the only yes, and
// both absent and null are no.
func hasCapability[T any](capability Opt[T]) bool {
	_, present := capability.Get()
	return present
}

// advertiseAgent derives an agent's advertisement from the methods it serves.
func advertiseAgent(serves func(method string) bool) AgentCapabilities {
	var advertised AgentCapabilities
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if capability, ok := gates[method].(agentCapability); ok && serves(method) {
			capability.set(&advertised)
		}
	}
	return advertised
}

// advertiseClient derives a client's advertisement from the methods it serves.
func advertiseClient(serves func(method string) bool) ClientCapabilities {
	var advertised ClientCapabilities
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if capability, ok := gates[method].(clientCapability); ok && capability.set != nil && serves(method) {
			capability.set(&advertised)
		}
	}
	return advertised
}

// unservedAgentMethods reports the methods a stated agent advertisement claims
// that its handlers do not serve, in a stable order.
func unservedAgentMethods(stated AgentCapabilities, serves func(method string) bool) []string {
	var unserved []string
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if capability, ok := gates[method].(agentCapability); ok && capability.get(stated) && !serves(method) {
			unserved = append(unserved, capability.path()+" advertises "+method+" without the handler that serves it")
		}
	}
	return unserved
}

// unservedClientMethods is [unservedAgentMethods] for a client.
func unservedClientMethods(stated ClientCapabilities, serves func(method string) bool) []string {
	var unserved []string
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if capability, ok := gates[method].(clientCapability); ok && capability.get(stated) && !serves(method) {
			unserved = append(unserved, capability.path()+" advertises "+method+" without the handler that serves it")
		}
	}
	return unserved
}
