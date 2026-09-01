package acp

import (
	"context"
	"strings"
	"testing"
)

// The capability table is hand-maintained, so it is the one thing in this package
// that can silently fall behind the schema. These hold it against the generated
// method table in both directions.
//
// This is the check design.md asks CI for. It runs as a test rather than as a
// script because the two things it compares are both in this package, and a test
// that fails on a schema bump names the method it is missing.
func TestEveryMethodHasAGate(t *testing.T) {
	for name := range standardMethods {
		if _, gated := gates[name]; !gated {
			t.Errorf("the schema defines %q and the capability table does not classify it; "+
				"add a row saying whether it is baseline, gated, or not implemented yet", name)
		}
	}
}

func TestNoGateNamesAMethodTheSchemaDoesNotDefine(t *testing.T) {
	for name := range gates {
		if !isStandardMethod(name) {
			t.Errorf("the capability table classifies %q, which the schema no longer defines", name)
		}
	}
}

// A row's shape has to match its classification, or the gate would either consult
// a predicate that is not there or ignore one that is.
func TestGateRowsAreWellFormed(t *testing.T) {
	for name, gate := range gates {
		switch gate.gating {
		case gatingCapability:
			if gate.advertised == nil {
				t.Errorf("%q is gated on a capability with no predicate to read it", name)
			}
			if gate.capability == "" {
				t.Errorf("%q is gated on a capability the row does not name, so a refusal cannot say which", name)
			}
		case gatingBaseline:
			if gate.advertised != nil {
				t.Errorf("%q is baseline and carries a predicate, so something reads a gate that is not there", name)
			}
			if gate.capability != "" {
				t.Errorf("%q is baseline and names a capability", name)
			}
		case gatingUnimplemented:
			// An unimplemented method may carry a predicate, and most do: it is
			// what construction reads to refuse an advertisement for a method
			// this package cannot serve. What it may not do is carry one without
			// naming the capability, because the refusal quotes that name.
			if gate.advertised != nil && gate.capability == "" {
				t.Errorf("%q has a predicate and no capability to name in the refusal", name)
			}
			if gate.advertised == nil && gate.capability != "" {
				t.Errorf("%q names a capability with no predicate to read it", name)
			}
		}
		if gate.advertised != nil && gate.owner == 0 && gate.capability != "" {
			// sideAgent is zero, so this only catches a row that forgot the field
			// entirely — which is why the check is that the capability path and
			// the owner agree rather than that the owner is set.
			if !strings.HasPrefix(gate.capability, "agentCapabilities") {
				t.Errorf("%q reads %s and its owner says the agent", name, gate.capability)
			}
		}
		if gate.owner == sideClient && !strings.HasPrefix(gate.capability, "clientCapabilities") {
			t.Errorf("%q reads %s and its owner says the client", name, gate.capability)
		}
		if gate.why == "" {
			t.Errorf("%q is classified without saying why, so the classification cannot be reviewed", name)
		}
	}
}

// Every predicate, exercised in both states. A predicate that read the wrong
// field would be an authority boundary in the wrong place, and nothing else here
// would notice.
func TestEachPredicateReadsItsOwnCapability(t *testing.T) {
	tests := []struct {
		method    string
		advertise func(*PeerInfo)
	}{
		{
			method:    methodSessionLoad,
			advertise: func(peer *PeerInfo) { peer.AgentCapabilities.LoadSession = true },
		},
		{
			method:    methodFsReadTextFile,
			advertise: func(peer *PeerInfo) { peer.ClientCapabilities.Fs.ReadTextFile = true },
		},
		{
			method:    methodFsWriteTextFile,
			advertise: func(peer *PeerInfo) { peer.ClientCapabilities.Fs.WriteTextFile = true },
		},
		{
			method:    methodTerminalCreate,
			advertise: func(peer *PeerInfo) { peer.ClientCapabilities.Terminal = true },
		},
	}

	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			var silent PeerInfo
			if err := silent.permits(test.method); err == nil {
				t.Error("allowed with nothing advertised")
			}

			var advertising PeerInfo
			test.advertise(&advertising)
			if err := advertising.permits(test.method); err != nil {
				t.Errorf("refused with the capability advertised: %v", err)
			}
		})
	}
}

// Reading and writing are two capabilities, and the terminal group is one. Both
// facts are the schema's, and both are easy to get wrong in a table.
func TestTheFilesystemCapabilitiesAreIndependent(t *testing.T) {
	var reading PeerInfo
	reading.ClientCapabilities.Fs.ReadTextFile = true

	if reading.permits(methodFsReadTextFile) != nil {
		t.Error("reading was refused although it was advertised")
	}
	if reading.permits(methodFsWriteTextFile) == nil {
		t.Error("advertising the read capability also allowed writing")
	}
}

func TestTheTerminalCapabilityCoversAllFiveMethods(t *testing.T) {
	terminalMethods := []string{
		methodTerminalCreate,
		methodTerminalOutput,
		methodTerminalWaitForExit,
		methodTerminalKill,
		methodTerminalRelease,
	}

	var advertising PeerInfo
	advertising.ClientCapabilities.Terminal = true
	for _, name := range terminalMethods {
		if advertising.permits(name) != nil {
			t.Errorf("%q was refused although the terminal capability was advertised", name)
		}
		var silent PeerInfo
		if silent.permits(name) == nil {
			t.Errorf("%q was allowed with the terminal capability unadvertised", name)
		}
	}
}

// Advertising a capability whose method this package does not implement is refused
// at construction, before a connection exists to break the promise on.
//
// This used to be a hole: only loadSession and the fs/terminal handlers were
// checked, so a peer could advertise an unsupported method and the inbound gate
// would then refuse a method the peer had been told was there.
func TestAdvertisingAnUnimplementedMethodIsRefusedAtConstruction(t *testing.T) {
	agentCases := map[string]*AgentCapabilities{
		"session listing": {SessionCapabilities: SessionCapabilities{List: OptValue(SessionListCapabilities{})}},
		"logout":          {Auth: AgentAuthCapabilities{Logout: OptValue(LogoutCapabilities{})}},
	}
	for name, capabilities := range agentCases {
		t.Run("an agent advertising "+name, func(t *testing.T) {
			_, err := NewAgent(&AgentConfig{
				NewSession:   newSessionStub,
				Prompt:       promptStub,
				Cancel:       func(context.Context, *AgentSession, *CancelNotification) {},
				Capabilities: capabilities,
			})
			if err == nil {
				t.Fatal("the advertisement was accepted, so the inbound gate would refuse a method " +
					"the peer was told was there")
			}
		})
	}

	t.Run("a client advertising elicitation", func(t *testing.T) {
		_, err := NewClient(&ClientConfig{
			SessionUpdate: func(context.Context, *SessionNotification) {},
			RequestPermission: func(context.Context, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
				return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
			},
			Capabilities: &ClientCapabilities{Elicitation: OptValue(ElicitationCapabilities{})},
		})
		if err == nil {
			t.Fatal("a client advertised elicitation, which this package does not implement")
		}
	})
}

// These capability fields explicitly define both omitted and null as false. Opt
// keeps those states separate for round trips, so the authority check must not
// confuse "present null" with "present capability object".
func TestNullCapabilityObjectsDoNotAdvertiseMethods(t *testing.T) {
	_, err := NewAgent(&AgentConfig{
		NewSession: newSessionStub,
		Prompt:     promptStub,
		Cancel:     func(context.Context, *AgentSession, *CancelNotification) {},
		Capabilities: &AgentCapabilities{
			Auth: AgentAuthCapabilities{Logout: OptNull[LogoutCapabilities]()},
			SessionCapabilities: SessionCapabilities{
				List:   OptNull[SessionListCapabilities](),
				Delete: OptNull[SessionDeleteCapabilities](),
				Resume: OptNull[SessionResumeCapabilities](),
				Close:  OptNull[SessionCloseCapabilities](),
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent rejected null capabilities as advertisements: %v", err)
	}

	_, err = NewClient(&ClientConfig{
		SessionUpdate: func(context.Context, *SessionNotification) {},
		RequestPermission: func(context.Context, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
			return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
		},
		Capabilities: &ClientCapabilities{Elicitation: OptNull[ElicitationCapabilities]()},
	})
	if err != nil {
		t.Fatalf("NewClient rejected a null elicitation capability as an advertisement: %v", err)
	}
}

// An agent that offers authentication methods must be able to serve the method it
// tells a client to call.
func TestOfferingAuthMethodsWithoutTheHandlerIsRefused(t *testing.T) {
	_, err := NewAgent(&AgentConfig{
		AuthMethods: []AuthMethod{&AuthMethodAgent{ID: "oauth", Name: "Sign in"}},
		NewSession:  newSessionStub,
		Prompt:      promptStub,
		Cancel:      func(context.Context, *AgentSession, *CancelNotification) {},
	})
	if err == nil {
		t.Fatal("an agent offered an authentication method it cannot serve")
	}
}

// A baseline method needs no capability, and a method this package does not serve
// yet is refused however much a peer advertises. The second is what keeps an
// unimplemented row from reading as an implemented one.
func TestBaselineIsAllowedAndUnimplementedIsNot(t *testing.T) {
	var silent PeerInfo
	for _, name := range []string{methodInitialize, methodSessionNew, methodSessionPrompt, methodSessionUpdate} {
		if silent.permits(name) != nil {
			t.Errorf("the baseline method %q was refused", name)
		}
	}
	for _, name := range []string{methodLogout, methodSessionList, methodElicitationCreate} {
		if silent.permits(name) == nil {
			t.Errorf("%q is not implemented yet but the gate allowed it", name)
		}
	}
}

// A name outside the table is not a standard method, and the gate is not where an
// extension method is decided.
func TestAnUnknownMethodIsRefused(t *testing.T) {
	var silent PeerInfo
	if silent.permits("_vendor.example/thing") == nil {
		t.Error("an extension method reached the capability gate and was allowed")
	}
	if isStandardMethod("_vendor.example/thing") {
		t.Error("an extension method is reported as standard, so the extension API would refuse it")
	}
}

// The generated table agrees with the schema about how many methods there are,
// which is the number the documentation quotes.
func TestTheMethodTableIsTheSizeTheSchemaSays(t *testing.T) {
	if got, want := len(standardMethods), 25; got != want {
		t.Fatalf("the method table has %d methods, want %d", got, want)
	}
	if standardMethods[methodCancelRequest].side != sideProtocol {
		t.Error("$/cancel_request belongs to the connection rather than to either peer")
	}
	if standardMethods[methodSessionUpdate].shape != shapeNotification {
		t.Error("session/update is a notification: there is no response to return")
	}
	if standardMethods[methodSessionPrompt].shape != shapeRequest {
		t.Error("session/prompt is a request, and the turn ends when it is answered")
	}
}

// Handlers for the tests above, which are about what construction refuses rather
// than about what a handler answers.
func newSessionStub(context.Context, *NewSessionRequest) (*NewSessionResponse, error) {
	return &NewSessionResponse{SessionID: "sess-1"}, nil
}

func promptStub(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
	return &PromptResponse{StopReason: StopReasonEndTurn}, nil
}
