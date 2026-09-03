package acp

import (
	"context"
	"errors"
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
	// These are implementable now, so what the construction check catches is an
	// advertisement with no handler behind it rather than one this package could
	// never serve.
	agentCases := map[string]*AgentCapabilities{
		"session listing": {SessionCapabilities: SessionCapabilities{List: OptValue(SessionListCapabilities{})}},
		"session closing": {SessionCapabilities: SessionCapabilities{Close: OptValue(SessionCloseCapabilities{})}},
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

	t.Run("a client advertising an elicitation mode it cannot render", func(t *testing.T) {
		_, err := NewClient(&ClientConfig{
			SessionUpdate:     func(context.Context, *SessionNotification) {},
			RequestPermission: permissionStub,
			Capabilities: &ClientCapabilities{Elicitation: OptValue(ElicitationCapabilities{
				Form: OptValue(ElicitationFormCapabilities{}),
			})},
		})
		if err == nil {
			t.Fatal("a client advertised the form mode with no handler that renders it, so every " +
				"form elicitation would be refused after the agent was told it could send one")
		}
	})

	// The mode is checked in the same direction as a method: advertising more than
	// the handlers serve is refused, advertising less is a client's own choice.
	t.Run("a client advertising one mode of two it serves", func(t *testing.T) {
		_, err := NewClient(&ClientConfig{
			SessionUpdate:     func(context.Context, *SessionNotification) {},
			RequestPermission: permissionStub,
			Elicitation: &ElicitationHandlers{
				Form: formStub,
				URL:  urlStub,
			},
			Capabilities: &ClientCapabilities{Elicitation: OptValue(ElicitationCapabilities{
				Form: OptValue(ElicitationFormCapabilities{}),
			})},
		})
		if err != nil {
			t.Fatalf("a client that renders both modes and offers one was refused: %v", err)
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

// A baseline method needs no capability, and a gated one is refused until the peer
// that serves it says it is there.
func TestBaselineIsAllowedAndGatedIsNotUntilAdvertised(t *testing.T) {
	var silent PeerInfo
	for _, name := range []string{methodInitialize, methodSessionNew, methodSessionPrompt, methodSessionUpdate} {
		if silent.permits(name) != nil {
			t.Errorf("the baseline method %q was refused", name)
		}
	}
	for _, name := range []string{
		methodLogout, methodSessionList, methodSessionClose,
		methodElicitationCreate, methodElicitationComplete,
	} {
		if silent.permits(name) == nil {
			t.Errorf("%q is gated but the gate allowed it unadvertised", name)
		}
	}

	var loud PeerInfo
	loud.ClientCapabilities.Elicitation = OptValue(ElicitationCapabilities{
		Form: OptValue(ElicitationFormCapabilities{}),
		URL:  OptValue(ElicitationURLCapabilities{}),
	})
	for _, name := range []string{methodElicitationCreate, methodElicitationComplete} {
		if err := loud.permits(name); err != nil {
			t.Errorf("%q was advertised and the gate still refused it: %v", name, err)
		}
	}

	// The url mode gates the completion on its own, so a client that renders forms
	// and not pages serves the one and not the other.
	var formOnly PeerInfo
	formOnly.ClientCapabilities.Elicitation = OptValue(ElicitationCapabilities{
		Form: OptValue(ElicitationFormCapabilities{}),
	})
	if err := formOnly.permits(methodElicitationCreate); err != nil {
		t.Errorf("a form-only client was refused elicitation/create: %v", err)
	}
	if formOnly.permits(methodElicitationComplete) == nil {
		t.Error("a client that advertised no url mode was allowed elicitation/complete")
	}
}

// The unimplemented classification, which no row uses now that elicitation is
// served. It is tested on a table of its own because the thing being checked is
// that the classification still means what capabilities.go says it means — the
// day a schema bump adds a method, this is the row somebody will reach for.
func TestAnUnimplementedRowIsRefusedToBothSides(t *testing.T) {
	const method = "example/not_served"
	table := gateTable{method: {
		gating:     gatingUnimplemented,
		capability: "clientCapabilities.example",
		owner:      sideClient,
		advertised: func(PeerInfo) bool { return true },
	}}

	// Construction: an advertisement is refused for the method, not for a missing
	// handler, because no handler could make it servable.
	exceeded := table.exceeded(PeerInfo{}, sideClient, func(string) bool { return true })
	if len(exceeded) != 1 {
		t.Fatalf("exceeded reported %v, want the one unimplemented method", exceeded)
	}
	if !strings.Contains(exceeded[0], "does not implement") {
		t.Errorf("exceeded said %q, which does not say the package cannot serve it", exceeded[0])
	}

	// And it is refused even when the implementation check would have passed,
	// which is what keeps an unimplemented row from reading as a gated one.
	if got := table.exceeded(PeerInfo{}, sideClient, func(string) bool { return false }); len(got) != 1 {
		t.Fatalf("exceeded reported %v with no handler either, want the same one", got)
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
func newSessionStub(context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
	return &NewSessionResponse{SessionID: "sess-1"}, nil
}

func promptStub(context.Context, *AgentSession, *PromptRequest) (*PromptResponse, error) {
	return &PromptResponse{StopReason: StopReasonEndTurn}, nil
}

// The boolean session configuration option, which the schema gates through the
// client's own advertisement rather than the agent's.
func TestABooleanConfigOptionNeedsTheClientToHaveAdvertisedIt(t *testing.T) {
	boolean := &SetSessionConfigOptionRequestBoolean{}
	selected := &SetSessionConfigOptionRequestValueID{}

	var silent PeerInfo
	if err := silent.permitsConfigOptionValue(selected); err != nil {
		t.Errorf("a select value was refused, and the schema gates only the boolean type: %v", err)
	}
	err := silent.permitsConfigOptionValue(boolean)
	if err == nil {
		t.Fatal("a boolean value was allowed although the client advertised no support for one")
	}
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != ErrorCodeInvalidParams {
		t.Fatalf("refused with %v, want invalid params: the method is there and it is the value "+
			"inside it that was never advertised", err)
	}
	if !strings.Contains(refusal.Message, "clientCapabilities.session.configOptions.boolean") {
		t.Errorf("the refusal says %q, which does not name the capability", refusal.Message)
	}

	// Every level of the nesting has to be present, because each is an Opt whose
	// absent state means the capability was not advertised.
	partial := PeerInfo{ClientCapabilities: ClientCapabilities{
		Session: OptValue(ClientSessionCapabilities{}),
	}}
	if partial.permitsConfigOptionValue(boolean) == nil {
		t.Error("session capabilities with no configOptions advertised a boolean option")
	}

	advertised := PeerInfo{ClientCapabilities: ClientCapabilities{
		Session: OptValue(ClientSessionCapabilities{
			ConfigOptions: OptValue(SessionConfigOptionsCapabilities{
				Boolean: OptValue(BooleanConfigOptionCapabilities{}),
			}),
		}),
	}}
	if err := advertised.permitsConfigOptionValue(boolean); err != nil {
		t.Errorf("a boolean value was refused although the client advertised it: %v", err)
	}
}

// The dual of TestEveryMethodHasAGate: every capability the schema defines is
// read by something.
//
// The method table says which capability gates which method, and a test holds it
// against the generated method list in both directions. Nothing said the same
// about capabilities, and the gap that left was real: the boolean configuration
// option capability was defined, decoded, carried in PeerInfo, and read by
// nothing at all.
//
// A capability that gates a method is read by the table. One that gates a
// parameter is read by a permits* function. One that carries no permission is
// listed here with what it does instead. A capability in none of the three is a
// grant this package accepted and never acted on.
func TestEveryCapabilityIsRead(t *testing.T) {
	gated := map[string]bool{}
	for _, gate := range gates {
		if gate.capability != "" {
			gated[gate.capability] = true
		}
	}

	// The leaves the schema defines, each with what reads it.
	leaves := map[string]string{
		"agentCapabilities.loadSession":                               "gates session/load",
		"agentCapabilities.auth.logout":                               "gates logout",
		"agentCapabilities.sessionCapabilities.list":                  "gates session/list",
		"agentCapabilities.sessionCapabilities.delete":                "gates session/delete",
		"agentCapabilities.sessionCapabilities.resume":                "gates session/resume",
		"agentCapabilities.sessionCapabilities.close":                 "gates session/close",
		"clientCapabilities.fs.readTextFile":                          "gates fs/read_text_file",
		"clientCapabilities.fs.writeTextFile":                         "gates fs/write_text_file",
		"clientCapabilities.terminal":                                 "gates all five terminal methods",
		"clientCapabilities.elicitation":                              "gates elicitation/create",
		"clientCapabilities.elicitation.url":                          "gates elicitation/complete",
		"agentCapabilities.promptCapabilities.image":                  "PeerInfo.permitsPromptContent",
		"agentCapabilities.promptCapabilities.audio":                  "PeerInfo.permitsPromptContent",
		"agentCapabilities.promptCapabilities.embeddedContext":        "PeerInfo.permitsPromptContent",
		"agentCapabilities.mcpCapabilities.http":                      "PeerInfo.permitsSessionSetup",
		"agentCapabilities.mcpCapabilities.sse":                       "PeerInfo.permitsSessionSetup",
		"agentCapabilities.sessionCapabilities.additionalDirectories": "PeerInfo.permitsSessionSetup",
		"clientCapabilities.elicitation.form":                         "PeerInfo.permitsElicitationMode",
		"clientCapabilities.session.configOptions.boolean":            "PeerInfo.permitsConfigOptionValue",
		"clientCapabilities.auth.terminal":                            "authenticationMethods.offered and validateOffer",
	}

	// Held against the gate table, so a row whose capability path is misspelled
	// cannot pass by being listed here as gating something.
	for path, how := range leaves {
		if strings.HasPrefix(how, "gates ") && !gated[path] {
			t.Errorf("%s is listed as gating a method, and no gate row names it", path)
		}
		if !strings.HasPrefix(how, "gates ") && gated[path] {
			t.Errorf("%s gates a method and is listed as read by %s instead", path, how)
		}
	}
	for path := range gated {
		if _, listed := leaves[path]; !listed {
			t.Errorf("the gate table reads %s, which this list does not name", path)
		}
	}
}

func permissionStub(
	context.Context,
	*RequestPermissionRequest,
) (*RequestPermissionResponse, error) {
	return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}, nil
}

func formStub(
	context.Context,
	*CreateElicitationRequest,
	*ElicitationFormMode,
) (*CreateElicitationResponse, error) {
	return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
}

func urlStub(
	context.Context,
	*CreateElicitationRequest,
	*ElicitationURLMode,
) (*CreateElicitationResponse, error) {
	return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
}

// A present capability object advertising no mode advertises nothing, so it
// promises nothing to break. The elicitation RFD is explicit that it "advertises
// no supported modes, not form support", and the method gate reads it the same
// way: an agent is not permitted a call every mode of which would be refused.
func TestAnEmptyElicitationObjectAdvertisesNoMode(t *testing.T) {
	empty := PeerInfo{ClientCapabilities: ClientCapabilities{
		Elicitation: OptValue(ElicitationCapabilities{}),
	}}
	if empty.permits(methodElicitationCreate) == nil {
		t.Error("a client advertising no mode was offered elicitation/create, so every call " +
			"it accepted would then be refused for its mode")
	}

	client, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *SessionNotification) {},
		RequestPermission: permissionStub,
		Capabilities: &ClientCapabilities{
			Elicitation: OptValue(ElicitationCapabilities{}),
		},
	})
	if err != nil {
		t.Fatalf("an empty elicitation object was refused, and it promises nothing: %v", err)
	}
	if anyElicitationMode(client.capabilities) {
		t.Error("an empty elicitation object was read as advertising a mode")
	}
}
