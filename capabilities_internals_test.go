package acp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// The capability table is hand-maintained, so it is the one thing that can
// silently fall behind the schema. These hold it against the generated method
// table and the schema's own capability tree.

func TestEveryMethodHasAGate(t *testing.T) {
	for name := range standardMethods {
		if _, classified := gates[name]; !classified {
			t.Errorf("the schema defines %q and the capability table does not classify it", name)
		}
	}
	for name := range gates {
		if !isStandardMethod(name) {
			t.Errorf("the capability table classifies %q, which the schema no longer defines", name)
		}
	}
}

// A gate reads the capabilities of the side that serves its method.
func TestEachGateReadsTheServingSide(t *testing.T) {
	for method, gate := range gates {
		side := standardMethods[method].side
		switch gate.(type) {
		case nil:
		case capabilityLeaf[AgentCapabilities]:
			if side != sideAgent {
				t.Errorf("%s is served by side %d and gated on an agent capability", method, side)
			}
		case capabilityLeaf[ClientCapabilities]:
			if side != sideClient {
				t.Errorf("%s is served by side %d and gated on a client capability", method, side)
			}
		default:
			t.Errorf("%s has a gate of unknown kind %T", method, gate)
		}
	}
}

// Advertising one capability permits exactly the methods that share its leaf. A
// closure reading or writing the wrong leaf would put an authority boundary in the
// wrong place, and nothing else would notice.
func TestEachCapabilityPermitsExactlyItsMethods(t *testing.T) {
	for method, gate := range gates {
		var agreed Negotiated
		switch gate := gate.(type) {
		case capabilityLeaf[AgentCapabilities]:
			gate.set(&agreed.AgentCapabilities)
		case capabilityLeaf[ClientCapabilities]:
			if gate.set == nil {
				continue // derived from a handler group; covered below
			}
			gate.set(&agreed.ClientCapabilities)
		default:
			continue
		}
		for other, otherGate := range gates {
			advertised := agreed.advertises(other)
			wanted := otherGate == nil || otherGate.path() == gate.path()
			if advertised != wanted {
				t.Errorf("advertising %s's capability made %s advertised = %t", method, other, advertised)
			}
		}
	}
}

func TestTheElicitationGatesFollowTheModes(t *testing.T) {
	var silent, empty, formOnly, both Negotiated
	empty.ClientCapabilities.Elicitation = OptValue(ElicitationCapabilities{})
	formOnly.ClientCapabilities.Elicitation = OptValue(ElicitationCapabilities{
		Form: OptValue(ElicitationFormCapabilities{}),
	})
	both.ClientCapabilities.Elicitation = OptValue(ElicitationCapabilities{
		Form: OptValue(ElicitationFormCapabilities{}),
		URL:  OptValue(ElicitationURLCapabilities{}),
	})
	for name, test := range map[string]struct {
		agreed           Negotiated
		create, complete bool
	}{
		// A present object advertising no mode advertises nothing: the RFD says it
		// "advertises no supported modes, not form support".
		"nothing":   {silent, false, false},
		"no mode":   {empty, false, false},
		"form only": {formOnly, true, false},
		"both":      {both, true, true},
	} {
		create := test.agreed.advertises(methodElicitationCreate)
		complete := test.agreed.advertises(methodElicitationComplete)
		if create != test.create || complete != test.complete {
			t.Errorf("%s: create %t complete %t, want %t and %t", name, create, complete, test.create, test.complete)
		}
	}
}

func TestAnUnknownMethodIsNotAdvertised(t *testing.T) {
	var agreed Negotiated
	if agreed.advertises("_vendor.example/thing") {
		t.Error("an extension method passed the capability gate")
	}
}

// Advertising a method no handler serves is refused at construction, before a
// connection exists to break the promise on.
func TestAdvertisingAnUnservedMethodIsRefusedAtConstruction(t *testing.T) {
	for name, capabilities := range map[string]*AgentCapabilities{
		"session listing": {SessionCapabilities: SessionCapabilities{List: OptValue(SessionListCapabilities{})}},
		"session closing": {SessionCapabilities: SessionCapabilities{Close: OptValue(SessionCloseCapabilities{})}},
		"logout":          {Auth: AgentAuthCapabilities{Logout: OptValue(LogoutCapabilities{})}},
	} {
		_, err := NewAgent(&AgentConfig{NewSession: newSessionHandler, Prompt: endTurn, Capabilities: capabilities})
		if err == nil {
			t.Errorf("an agent advertising %s with no handler was accepted", name)
		}
	}

	_, err := NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *ClientConn, *SessionNotification) {},
		RequestPermission: permissionStub,
		Capabilities: &ClientCapabilities{Elicitation: OptValue(ElicitationCapabilities{
			Form: OptValue(ElicitationFormCapabilities{}),
		})},
	})
	if err == nil {
		t.Error("a client advertised the form mode with no handler that renders it")
	}

	// Advertising fewer modes than the handlers serve is the client's choice.
	_, err = NewClient(&ClientConfig{
		SessionUpdate:     func(context.Context, *ClientConn, *SessionNotification) {},
		RequestPermission: permissionStub,
		Elicitation:       &ElicitationHandlers{Form: formStub, URL: urlStub},
		Capabilities: &ClientCapabilities{Elicitation: OptValue(ElicitationCapabilities{
			Form: OptValue(ElicitationFormCapabilities{}),
		})},
	})
	if err != nil {
		t.Errorf("a client that renders both modes and offers one was refused: %v", err)
	}
}

// Capability objects define both omitted and null as no, so a null object is not
// an advertisement to refuse.
func TestNullCapabilityObjectsDoNotAdvertiseMethods(t *testing.T) {
	_, err := NewAgent(&AgentConfig{
		NewSession: newSessionHandler,
		Prompt:     endTurn,
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
}

func TestOfferingAuthMethodsWithoutTheHandlerIsRefused(t *testing.T) {
	_, err := NewAgent(&AgentConfig{
		AuthMethods: []AuthMethod{&AuthMethodAgent{ID: "oauth", Name: "Sign in"}},
		NewSession:  newSessionHandler,
		Prompt:      endTurn,
	})
	if err == nil {
		t.Fatal("an agent offered an authentication method it cannot serve")
	}
}

// The boolean configuration option is granted through the client's own
// advertisement, at every level of its nesting.
func TestABooleanConfigOptionNeedsTheClientToHaveAdvertisedIt(t *testing.T) {
	boolean := &SetSessionConfigOptionRequest{Value: &SetSessionConfigOptionRequestBoolean{}}
	selected := &SetSessionConfigOptionRequest{Value: &SetSessionConfigOptionRequestValueID{}}

	var silent Negotiated
	if err := selected.checkOutbound(&silent); err != nil {
		t.Errorf("a select value was refused, and the schema gates only the boolean type: %v", err)
	}
	err := boolean.checkOutbound(&silent)
	if !errors.Is(err, ErrNotAdvertised) || !strings.Contains(err.Error(),
		"clientCapabilities.session.configOptions.boolean") {
		t.Fatalf("refused with %v, want ErrNotAdvertised naming the capability", err)
	}
	partial := Negotiated{ClientCapabilities: ClientCapabilities{Session: OptValue(ClientSessionCapabilities{})}}
	if boolean.checkOutbound(&partial) == nil {
		t.Error("session capabilities with no configOptions advertised a boolean option")
	}
	advertised := Negotiated{ClientCapabilities: ClientCapabilities{
		Session: OptValue(ClientSessionCapabilities{
			ConfigOptions: OptValue(SessionConfigOptionsCapabilities{
				Boolean: OptValue(BooleanConfigOptionCapabilities{}),
			}),
		}),
	}}
	if err := boolean.checkOutbound(&advertised); err != nil {
		t.Errorf("a boolean value was refused although the client advertised it: %v", err)
	}
}

// Every capability leaf the schema defines is read by something: a method gate,
// a parameter capability, or the one reader named here. The leaves come from the
// schema itself, so a capability added upstream fails this test until something
// acts on it, rather than arriving as a grant nothing enforces.
func TestEveryCapabilityIsRead(t *testing.T) {
	read := map[string]bool{
		// Read by authentication: terminal methods are offered only to a client
		// that enabled them.
		"clientCapabilities.auth.terminal": true,
	}
	for _, gate := range gates {
		if gate != nil {
			read[gate.path()] = true
		}
	}
	for _, capability := range []parameterCapability{
		promptImage, promptAudio, promptResource, mcpHTTP, mcpSse, additionalDirectories, systemPrompt,
		systemPromptOverride, booleanConfigOptions, noticeUpdates, compactionUpdates, planUpdates,
		subagentUpdates, elicitationForm.parameterCapability, elicitationURL.parameterCapability,
	} {
		read[capability.capability] = true
	}

	leaves, nodes := schemaCapabilityTree(t)
	for _, leaf := range leaves {
		if !read[leaf] {
			t.Errorf("the schema defines %s and nothing in this package reads it", leaf)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(read)) {
		if !slices.Contains(nodes, path) {
			t.Errorf("this package reads %s, which the schema does not define", path)
		}
	}
}

// schemaCapabilityTree walks the schema's capability tree from both roots,
// returning its leaves and every path in it. A leaf is a boolean or an object
// whose only property is _meta, which the schema uses as a present-means-yes
// marker; a container may also be read for its own presence.
func schemaCapabilityTree(t *testing.T) (leaves, nodes []string) {
	t.Helper()
	raw, err := os.ReadFile("schema/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Defs map[string]capabilitySchema `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, schema capabilitySchema)
	walk = func(path string, schema capabilitySchema) {
		nodes = append(nodes, path)
		if ref := schema.reference(); ref != "" {
			schema = document.Defs[ref]
		}
		properties := slices.DeleteFunc(slices.Sorted(maps.Keys(schema.Properties)),
			func(name string) bool { return name == "_meta" })
		if len(properties) == 0 {
			leaves = append(leaves, path)
			return
		}
		for _, name := range properties {
			walk(path+"."+name, schema.Properties[name])
		}
	}
	for root, definition := range map[string]string{
		"agentCapabilities": "AgentCapabilities", "clientCapabilities": "ClientCapabilities",
	} {
		walk(root, document.Defs[definition])
	}
	slices.Sort(leaves)
	return leaves, nodes
}

type capabilitySchema struct {
	Ref        string                      `json:"$ref"`
	AllOf      []capabilitySchema          `json:"allOf"`
	AnyOf      []capabilitySchema          `json:"anyOf"`
	Properties map[string]capabilitySchema `json:"properties"`
}

func (s capabilitySchema) reference() string {
	for _, candidate := range append(append([]capabilitySchema{s}, s.AllOf...), s.AnyOf...) {
		if name, local := strings.CutPrefix(candidate.Ref, "#/$defs/"); local {
			return name
		}
	}
	return ""
}

func TestTheMethodTableIsTheSizeTheSchemaSays(t *testing.T) {
	if got, want := len(standardMethods), 25; got != want {
		t.Fatalf("the method table has %d methods, want %d", got, want)
	}
	if standardMethods[methodCancelRequest].side != sideProtocol {
		t.Error("$/cancel_request belongs to the connection rather than to either peer")
	}
	if !standardMethods[methodSessionDelete].requiresSessionID || standardMethods[methodSessionList].requiresSessionID {
		t.Error("session scoping does not follow the schema's params")
	}
}

func permissionStub(context.Context, *ClientConn, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
	return cancelledPermission(), nil
}

func formStub(
	context.Context, *ClientConn, *CreateElicitationRequest, *ElicitationFormMode,
) (*CreateElicitationResponse, error) {
	return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
}

func urlStub(
	context.Context, *ClientConn, *CreateElicitationRequest, *ElicitationURLMode,
) (*CreateElicitationResponse, error) {
	return &CreateElicitationResponse{Value: &ElicitationAcceptAction{}}, nil
}
