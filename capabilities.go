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

// A capabilityLeaf is one leaf of [AgentCapabilities] or [ClientCapabilities].
// set is nil where the advertisement is derived from a handler group rather than
// from one handler.
type capabilityLeaf[Capabilities any] struct {
	leafPath string
	// of picks the side of the agreement the leaf belongs to.
	of  func(*Negotiated) Capabilities
	get func(Capabilities) bool
	set func(*Capabilities)
}

func (c capabilityLeaf[Capabilities]) path() string { return c.leafPath }

func (c capabilityLeaf[Capabilities]) advertisedBy(agreed *Negotiated) bool {
	return c.get(c.of(agreed))
}

func agentLeaf(
	leaf string,
	get func(AgentCapabilities) bool,
	set func(*AgentCapabilities),
) capabilityLeaf[AgentCapabilities] {
	return capabilityLeaf[AgentCapabilities]{
		leafPath: "agentCapabilities." + leaf,
		of:       func(agreed *Negotiated) AgentCapabilities { return agreed.AgentCapabilities },
		get:      get,
		set:      set,
	}
}

func clientLeaf(
	leaf string,
	get func(ClientCapabilities) bool,
	set func(*ClientCapabilities),
) capabilityLeaf[ClientCapabilities] {
	return capabilityLeaf[ClientCapabilities]{
		leafPath: "clientCapabilities." + leaf,
		of:       func(agreed *Negotiated) ClientCapabilities { return agreed.ClientCapabilities },
		get:      get,
		set:      set,
	}
}

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

	methodSessionLoad: agentLeaf("loadSession",
		func(c AgentCapabilities) bool { return c.LoadSession },
		func(c *AgentCapabilities) { c.LoadSession = true }),
	methodLogout: agentLeaf("auth.logout",
		func(c AgentCapabilities) bool { return hasCapability(c.Auth.Logout) },
		func(c *AgentCapabilities) { c.Auth.Logout = OptValue(LogoutCapabilities{}) }),
	methodSessionList: agentLeaf("sessionCapabilities.list",
		func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.List) },
		func(c *AgentCapabilities) { c.SessionCapabilities.List = OptValue(SessionListCapabilities{}) }),
	methodSessionDelete: agentLeaf("sessionCapabilities.delete",
		func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Delete) },
		func(c *AgentCapabilities) { c.SessionCapabilities.Delete = OptValue(SessionDeleteCapabilities{}) }),
	methodSessionResume: agentLeaf("sessionCapabilities.resume",
		func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Resume) },
		func(c *AgentCapabilities) { c.SessionCapabilities.Resume = OptValue(SessionResumeCapabilities{}) }),
	methodSessionClose: agentLeaf("sessionCapabilities.close",
		func(c AgentCapabilities) bool { return hasCapability(c.SessionCapabilities.Close) },
		func(c *AgentCapabilities) { c.SessionCapabilities.Close = OptValue(SessionCloseCapabilities{}) }),

	methodFsReadTextFile: clientLeaf("fs.readTextFile",
		func(c ClientCapabilities) bool { return c.Fs.ReadTextFile },
		func(c *ClientCapabilities) { c.Fs.ReadTextFile = true }),
	methodFsWriteTextFile: clientLeaf("fs.writeTextFile",
		func(c ClientCapabilities) bool { return c.Fs.WriteTextFile },
		func(c *ClientCapabilities) { c.Fs.WriteTextFile = true }),
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
	methodElicitationCreate:   clientLeaf("elicitation", anyElicitationMode, nil),
	methodElicitationComplete: clientLeaf(elicitationURL.leaf, elicitationURL.advertisedIn, nil),
}

var terminalCapability = clientLeaf("terminal",
	func(c ClientCapabilities) bool { return c.Terminal },
	func(c *ClientCapabilities) { c.Terminal = true })

// hasCapability reads a capability object: a present `{}` is the only yes, and
// both absent and null are no.
func hasCapability[T any](capability Opt[T]) bool {
	_, present := capability.Get()
	return present
}

// advertise derives one side's advertisement from the methods it serves.
func advertise[Capabilities any](serves func(method string) bool) Capabilities {
	var advertised Capabilities
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if leaf, ok := gates[method].(capabilityLeaf[Capabilities]); ok && leaf.set != nil && serves(method) {
			leaf.set(&advertised)
		}
	}
	return advertised
}

// unservedMethods reports the methods a stated advertisement claims that its
// handlers do not serve, in a stable order.
func unservedMethods[Capabilities any](stated Capabilities, serves func(method string) bool) []string {
	var unserved []string
	for _, method := range slices.Sorted(maps.Keys(gates)) {
		if leaf, ok := gates[method].(capabilityLeaf[Capabilities]); ok && leaf.get(stated) && !serves(method) {
			unserved = append(unserved, leaf.path()+" advertises "+method+" without the handler that serves it")
		}
	}
	return unserved
}
