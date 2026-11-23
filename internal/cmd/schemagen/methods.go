package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"strconv"
)

// The x-side values, which are also the names of meta.json's three directories.
const (
	sideNameAgent    = "agent"
	sideNameClient   = "client"
	sideNameProtocol = "protocol"
)

// sideConstants maps each x-side value to the Go constant the table emits.
var sideConstants = map[string]string{
	sideNameAgent:    "sideAgent",
	sideNameClient:   "sideClient",
	sideNameProtocol: "sideProtocol",
}

// metaVersion is the meta.json layout this generator reads.
const metaVersion = 1

// A MethodTable is meta.json: the protocol's method directory, keyed by an
// identifier and valued by the wire name.
type MethodTable struct {
	Version         int               `json:"version"`
	AgentMethods    map[string]string `json:"agentMethods"`
	ClientMethods   map[string]string `json:"clientMethods"`
	ProtocolMethods map[string]string `json:"protocolMethods"`
}

// A methodShape is whether a method expects a response.
type methodShape int

const (
	shapeRequest methodShape = iota
	shapeNotification
)

// goConstant is the name the emitted table gives the shape.
func (s methodShape) goConstant() string {
	if s == shapeNotification {
		return "shapeNotification"
	}
	return "shapeRequest"
}

// A Method is one entry of the generated table.
type Method struct {
	// Name is the wire name: "session/prompt".
	Name string
	// GoName is the unexported constant, from meta.json's identifier for the
	// method, because that identifier is what upstream chose to call it.
	GoName            string
	Side              string
	Shape             methodShape
	RequiresSessionID bool
	// Payloads are the definitions annotated with this method.
	Payloads []string
}

// The schema's routing unions: the envelopes whose params or result list every
// payload of their kind. They are what says whether a payload is a request, a
// notification or a response.
var (
	requestEnvelopes      = []string{"ClientRequest", "AgentRequest"}
	notificationEnvelopes = []string{"ClientNotification", "AgentNotification"}
	responseEnvelopes     = []string{"ClientResponse", "AgentResponse"}
)

// sessionIDDefinition is the definition a payload's required property refers to
// when the payload is scoped to a session.
const sessionIDDefinition = "SessionId"

// planMethods builds the method table from meta.json and cross-checks it against
// the schema's own annotations and routing unions. A disagreement between them
// stops generation.
func planMethods(doc *Document, table *MethodTable) ([]*Method, error) {
	if table.Version != metaVersion {
		return nil, fmt.Errorf("meta.json declares version %d, and only %d is implemented", table.Version, metaVersion)
	}

	byName := make(map[string]*Method)
	for _, group := range []struct {
		side    string
		methods map[string]string
	}{
		{sideNameAgent, table.AgentMethods},
		{sideNameClient, table.ClientMethods},
		{sideNameProtocol, table.ProtocolMethods},
	} {
		for _, identifier := range sortedKeys(group.methods) {
			name := group.methods[identifier]
			if existing, seen := byName[name]; seen {
				return nil, fmt.Errorf("meta.json lists %q under both %s and %s, and a method served by "+
					"either peer is not implemented", name, existing.Side, group.side)
			}
			byName[name] = &Method{Name: name, GoName: "method" + goName(identifier), Side: group.side}
		}
	}

	for _, name := range sortedKeys(doc.Defs) {
		annotated := doc.Defs[name]
		if annotated.Method == "" {
			continue
		}
		method, listed := byName[annotated.Method]
		if !listed {
			return nil, fmt.Errorf("%s%s is annotated with method %q, which meta.json does not list",
				definitionPointer, name, annotated.Method)
		}
		if annotated.Side != method.Side {
			return nil, fmt.Errorf("meta.json lists %q as %s but %s says x-side %s",
				method.Name, method.Side, name, annotated.Side)
		}
		method.Payloads = append(method.Payloads, name)
	}

	routes := routingPayloads(doc)
	methods := make([]*Method, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		method := byName[name]
		if err := finishMethod(doc, method, routes); err != nil {
			return nil, err
		}
		methods = append(methods, method)
	}
	return methods, nil
}

// payloadRoutes is which routing union lists each payload.
type payloadRoutes struct {
	requests, notifications, responses map[string]bool
}

func routingPayloads(doc *Document) payloadRoutes {
	collect := func(envelopes []string, property string) map[string]bool {
		listed := make(map[string]bool)
		for _, envelope := range envelopes {
			if schema := doc.Defs[envelope]; schema != nil {
				for _, name := range envelopeRefs(schema, property) {
					listed[name] = true
				}
			}
		}
		return listed
	}
	return payloadRoutes{
		requests:      collect(requestEnvelopes, "params"),
		notifications: collect(notificationEnvelopes, "params"),
		responses:     collect(responseEnvelopes, "result"),
	}
}

// envelopeRefs returns the definitions an envelope's property, in any of the
// envelope's arms, refers to.
func envelopeRefs(envelope *Schema, property string) []string {
	var refs []string
	for _, arm := range append([]*Schema{envelope}, envelope.Arms()...) {
		if schema := arm.Properties[property]; schema != nil {
			refs = append(refs, collectRefs(schema)...)
		}
	}
	return refs
}

func collectRefs(schema *Schema) []string {
	if schema == nil {
		return nil
	}
	refs := []string{}
	if name := schema.RefName(); name != "" {
		refs = append(refs, name)
	}
	for _, list := range [][]*Schema{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, child := range list {
			refs = append(refs, collectRefs(child)...)
		}
	}
	return refs
}

func finishMethod(doc *Document, method *Method, routes payloadRoutes) error {
	if len(method.Payloads) == 0 {
		return fmt.Errorf("meta.json lists %q, which no payload in the schema is annotated with", method.Name)
	}

	// A method has a response exactly when one of its payloads is listed as a
	// response result. A request payload without one, or a notification payload
	// with one, contradicts the routing unions.
	var request, notification, response bool
	var params []string
	for _, payload := range method.Payloads {
		switch {
		case routes.responses[payload]:
			response = true
		case routes.requests[payload]:
			request = true
			params = append(params, payload)
		case routes.notifications[payload]:
			notification = true
			params = append(params, payload)
		default:
			// A protocol-level payload is routed by no envelope.
			params = append(params, payload)
		}
	}
	switch {
	case request && notification:
		return fmt.Errorf("%q is routed both as a request and as a notification, which is not implemented",
			method.Name)
	case request && !response:
		return fmt.Errorf("%q is routed as a request but no response payload is annotated with it", method.Name)
	case notification && response:
		return fmt.Errorf("%q is routed as a notification but has a response payload", method.Name)
	case response:
		method.Shape = shapeRequest
	default:
		method.Shape = shapeNotification
	}

	for _, payload := range params {
		schema := doc.Defs[payload]
		for _, property := range schema.Required {
			if schema.Properties[property].SoleRef() == sessionIDDefinition {
				method.RequiresSessionID = true
			}
		}
	}
	return nil
}

// emitMethods writes the method table. Every name in it is unexported: a caller
// names a method by calling the operation for it, and the extension API checks a
// string against this table rather than looking it up in it.
func emitMethods(methods []*Method, table *MethodTable) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(comment("", []string{
		generatedHeader,
		"",
		fmt.Sprintf("Source: schema/meta.json, version %d, cross-checked against the x-method and", table.Version),
		"x-side annotations and the routing unions in schema/schema.json.",
	}))
	out.WriteString("\npackage acp\n\n")

	out.WriteString("// The wire names of the methods the specification defines.\nconst (\n")
	for _, method := range methods {
		fmt.Fprintf(&out, "\t%s = %s\n", method.GoName, strconv.Quote(method.Name))
	}
	out.WriteString(")\n\n")

	out.WriteString("// A methodSide is which peer serves a method.\ntype methodSide uint8\n\nconst (\n")
	out.WriteString("\t// sideAgent is served by the agent, so a client sends it.\n\tsideAgent methodSide = iota\n")
	out.WriteString("\t// sideClient is served by the client, so an agent sends it.\n\tsideClient\n")
	out.WriteString("\t// sideProtocol belongs to the connection rather than to either peer.\n\tsideProtocol\n)\n\n")

	out.WriteString("// A methodShape is whether a method expects a response.\ntype methodShape uint8\n\nconst (\n")
	out.WriteString("\t// shapeRequest expects a response.\n\tshapeRequest methodShape = iota\n")
	out.WriteString("\t// shapeNotification has none.\n\tshapeNotification\n)\n\n")

	out.WriteString("type methodDescriptor struct {\n\tside              methodSide\n\tshape             methodShape\n\trequiresSessionID bool\n}\n\n")

	out.WriteString(comment("", []string{
		"standardMethods is every method the specification defines, and the set the",
		"extension API reserves: a standard method has exactly one path through the",
		"typed codec, the capability gate and the session binding.",
	}))
	out.WriteString("var standardMethods = map[string]methodDescriptor{\n")
	for _, method := range methods {
		fmt.Fprintf(&out, "\t%s: {side: %s, shape: %s", method.GoName, sideConstants[method.Side],
			method.Shape.goConstant())
		if method.RequiresSessionID {
			out.WriteString(", requiresSessionID: true")
		}
		out.WriteString("},\n")
	}
	out.WriteString("}\n\n")

	out.WriteString("// isStandardMethod reports whether the specification defines name.\n")
	out.WriteString("func isStandardMethod(name string) bool {\n")
	out.WriteString("\t_, standard := standardMethods[name]\n\treturn standard\n}\n")

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("the generated method table does not parse: %w", err)
	}
	return formatted, nil
}

func loadMethodTable(data []byte) (*MethodTable, error) {
	var table MethodTable
	if err := strictDecode(data, &table); err != nil {
		return nil, err
	}
	if len(table.AgentMethods) == 0 || len(table.ClientMethods) == 0 {
		return nil, errors.New("meta.json lists no methods for one of the two peers")
	}
	return &table, nil
}
