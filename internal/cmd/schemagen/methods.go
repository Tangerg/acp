package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"slices"
	"strconv"
	"strings"
)

// The x-side values, which are also the names of meta.json's three directories.
// Three passes compare against them: the directory a method is listed in, the
// annotation its payloads carry, and the Go constant the table emits.
const (
	sideNameAgent    = "agent"
	sideNameClient   = "client"
	sideNameProtocol = "protocol"
	sideNameBoth     = "both"
)

// A MethodTable is meta.json: the protocol's method directory, keyed by an
// identifier and valued by the wire name.
type MethodTable struct {
	Version         int               `json:"version"`
	AgentMethods    map[string]string `json:"agentMethods"`
	ClientMethods   map[string]string `json:"clientMethods"`
	ProtocolMethods map[string]string `json:"protocolMethods"`
}

// A Method is one entry of the generated table.
type Method struct {
	// Name is the wire name: "session/prompt".
	Name string
	// GoName is the unexported constant, from meta.json's identifier for the
	// method rather than from the wire name, because that identifier is what
	// upstream chose to call it.
	GoName            string
	Side              string
	Shape             string
	RequiresSessionID bool
	// Payloads are the definitions annotated with this method, which is what the
	// cross-check against meta.json is made of.
	Payloads []string
}

// planMethods builds the method table from meta.json and cross-checks it against
// the schema's own annotations.
//
// Neither source is sufficient alone. meta.json is the method directory but says
// nothing about whether a method expects a response; the schema's x-method and
// x-side annotations say that but are spread across 74 payload definitions. So
// both are read, and a disagreement stops generation — which is the check
// design.md asks CI for, made structural instead of periodic.
func planMethods(doc *Document, table *MethodTable) ([]*Method, error) {
	if table.Version != 1 {
		return nil, fmt.Errorf("meta.json declares version %d, and only 1 is implemented", table.Version)
	}

	// The wire name is the identity. A method listed under both directions is one
	// method that either peer may send, not two that share a name.
	byName := make(map[string]*Method)
	sides := map[string][]string{sideNameAgent: nil, sideNameClient: nil, sideNameProtocol: nil}
	for _, group := range []struct {
		side    string
		methods map[string]string
	}{
		{sideNameAgent, table.AgentMethods},
		{sideNameClient, table.ClientMethods},
		{sideNameProtocol, table.ProtocolMethods},
	} {
		for identifier, name := range group.methods {
			sides[group.side] = append(sides[group.side], name)
			method, seen := byName[name]
			if !seen {
				method = &Method{Name: name, GoName: "method" + goName(identifier)}
				byName[name] = method
			}
			if method.GoName != "method"+goName(identifier) {
				return nil, fmt.Errorf("meta.json gives %q two identifiers, %s and method%s",
					name, method.GoName, goName(identifier))
			}
		}
	}

	for _, name := range sortedKeys(doc.Defs) {
		annotated := doc.Defs[name]
		if annotated.Method == "" {
			continue
		}
		method, listed := byName[annotated.Method]
		if !listed {
			return nil, fmt.Errorf("#/$defs/%s is annotated with method %q, which meta.json does not list",
				name, annotated.Method)
		}
		method.Payloads = append(method.Payloads, name)
		if method.Side == "" {
			method.Side = annotated.Side
		}
		if method.Side != annotated.Side {
			return nil, fmt.Errorf("the payloads of %q disagree about x-side: %s and %s",
				annotated.Method, method.Side, annotated.Side)
		}
	}

	methods := make([]*Method, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		method := byName[name]
		if err := finishMethod(doc, method, sides); err != nil {
			return nil, err
		}
		methods = append(methods, method)
	}
	return methods, nil
}

func finishMethod(doc *Document, method *Method, sides map[string][]string) error {
	if len(method.Payloads) == 0 {
		return fmt.Errorf("meta.json lists %q, which no payload in the schema is annotated with", method.Name)
	}

	// The directory meta.json puts a method in and the x-side its payloads carry
	// are two statements of one fact, and a release where they part company is one
	// this generator should not paper over.
	inAgent := slices.Contains(sides[sideNameAgent], method.Name)
	inClient := slices.Contains(sides[sideNameClient], method.Name)
	expected := sideNameProtocol
	switch {
	case inAgent && inClient:
		expected = sideNameBoth
	case inAgent:
		expected = sideNameAgent
	case inClient:
		expected = sideNameClient
	}
	if method.Side != expected {
		return fmt.Errorf("meta.json lists %q as %s but its payloads say x-side %s",
			method.Name, expected, method.Side)
	}

	// Whether a method expects a response is not stated anywhere; it follows from
	// which payloads exist. A method with a response payload is a request, one
	// with a notification payload is a notification, and one the schema defines
	// both ways is either.
	var request, notification bool
	for _, payload := range method.Payloads {
		switch {
		case strings.HasSuffix(payload, "Notification"):
			notification = true
		case strings.HasSuffix(payload, "Response"):
			request = true
		}
		if !strings.HasSuffix(payload, "Response") &&
			slices.Contains(doc.Defs[payload].Required, "sessionId") {
			method.RequiresSessionID = true
		}
	}
	switch {
	case request && notification:
		method.Shape = "shapeEither"
	case request:
		method.Shape = "shapeRequest"
	case notification:
		method.Shape = "shapeNotification"
	default:
		return fmt.Errorf("the payloads of %q are %v, none of which says whether it expects a response",
			method.Name, method.Payloads)
	}
	return nil
}

// emitMethods writes the method table.
//
// Every name in it is unexported. A caller of this package names a method by
// calling the operation for it, never by spelling the string; the one place a
// string crosses the boundary is the extension API, and there it is checked
// against this table rather than looked up in it.
func emitMethods(methods []*Method, table *MethodTable) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(comment("", []string{
		generatedHeader,
		"",
		fmt.Sprintf("Source: schema/meta.json, version %d, cross-checked against the x-method and", table.Version),
		"x-side annotations in schema/schema.json.",
	}))
	out.WriteString("\npackage acp\n\n")

	out.WriteString(comment("", []string{
		"The wire names of the methods the specification defines. Unexported because a",
		"caller names a method by calling the operation for it: the method strings, the",
		"request identifiers and the envelope are all plumbing, and a caller who has to",
		"know them has been handed the plumbing.",
	}))
	out.WriteString("const (\n")
	for _, method := range methods {
		fmt.Fprintf(&out, "\t%s = %s\n", method.GoName, strconv.Quote(method.Name))
	}
	out.WriteString(")\n\n")

	out.WriteString(comment("", []string{
		"A methodSide is which peer serves a method. Both directions carry requests:",
		"fourteen of the specification's methods run from the agent to the client, so",
		"this is not a client library with a server mode bolted on.",
	}))
	out.WriteString("type methodSide uint8\n\nconst (\n")
	out.WriteString("\t// sideAgent is served by the agent, so a client sends it.\n\tsideAgent methodSide = iota\n")
	out.WriteString("\t// sideClient is served by the client, so an agent sends it.\n\tsideClient\n")
	out.WriteString("\t// sideBoth may be sent by either peer.\n\tsideBoth\n")
	out.WriteString("\t// sideProtocol belongs to the connection rather than to either peer.\n\tsideProtocol\n)\n\n")

	out.WriteString(comment("", []string{
		"A methodShape is whether a method expects a response. The schema does not say",
		"so directly; it follows from which payloads it defines for the method.",
	}))
	out.WriteString("type methodShape uint8\n\nconst (\n")
	out.WriteString("\t// shapeRequest expects a response.\n\tshapeRequest methodShape = iota\n")
	out.WriteString("\t// shapeNotification has none.\n\tshapeNotification\n")
	out.WriteString("\t// shapeEither is defined both ways, so neither form contradicts the schema.\n\tshapeEither\n)\n\n")

	out.WriteString("type methodDescriptor struct {\n\tside              methodSide\n\tshape             methodShape\n\trequiresSessionID bool\n}\n\n")

	out.WriteString(comment("", []string{
		"standardMethods is every method the specification defines, and the set the",
		"extension API reserves.",
		"",
		"An unrestricted method string on Call and Notify would be a hole through every",
		"invariant this package has: a caller could pass session/prompt and bypass the",
		"generated params type, the outbound validation, the session-ID binding and the",
		"capability gate. A standard method has exactly one path through the typed codec,",
		"and this is what says which names those are.",
	}))
	out.WriteString("var standardMethods = map[string]methodDescriptor{\n")
	for _, method := range methods {
		fmt.Fprintf(&out, "\t%s: {side: %s, shape: %s", method.GoName, sideConstant(method.Side), method.Shape)
		if method.RequiresSessionID {
			out.WriteString(", requiresSessionID: true")
		}
		out.WriteString("},\n")
	}
	out.WriteString("}\n\n")

	out.WriteString(comment("", []string{
		"isStandardMethod reports whether the specification defines name, which is what",
		"the extension API refuses and what a fallback handler is not offered.",
	}))
	out.WriteString("func isStandardMethod(name string) bool {\n")
	out.WriteString("\t_, standard := standardMethods[name]\n\treturn standard\n}\n")

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("the generated method table does not parse: %w", err)
	}
	return formatted, nil
}

func sideConstant(side string) string {
	return map[string]string{
		sideNameAgent:    "sideAgent",
		sideNameClient:   "sideClient",
		sideNameBoth:     "sideBoth",
		sideNameProtocol: "sideProtocol",
	}[side]
}

func loadMethodTable(data []byte) (*MethodTable, error) {
	var table MethodTable
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, err
	}
	if len(table.AgentMethods) == 0 || len(table.ClientMethods) == 0 {
		return nil, errors.New("meta.json lists no methods for one of the two peers")
	}
	return &table, nil
}
