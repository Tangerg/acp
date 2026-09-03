package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"strconv"
)

// emitRoundTrip writes the list of generated types the codec property test walks.
//
// The list is generated because the property is about all of them. A hand-written
// list is a second description of the plan that drifts the moment a schema bump
// adds a definition, and the type that arrives without coverage is exactly the one
// nobody would think to add. Generating it means a new definition is walked by the
// property on the same commit that introduces it.
//
// Interfaces are left out because there is no value to construct: a union's arms
// are in the list under their own names, which is where its codec is reached from
// anyway. So are the null arm, which carries nothing, and the unconstrained
// extension payload, whose grammar belongs to whoever agreed on it.
func emitRoundTrip(plan *Plan) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(comment("", []string{
		generatedHeader,
		"",
		fmt.Sprintf("Source: schema/schema.json, %s.", plan.SchemaTag),
	}))
	out.WriteString("\npackage acp_test\n\nimport \"github.com/Tangerg/acp\"\n\n")

	out.WriteString(comment("", []string{
		"generatedValues is one freshly allocated value per generated type, which is",
		"what roundtrip_test.go walks.",
		"",
		"New returns a new value every call rather than a shared one, because the",
		"property decodes into a second value and compares what both encode: reusing",
		"one would compare a value with itself.",
	}))
	out.WriteString("var generatedValues = []struct {\n\tName string\n\tNew  func() any\n}{\n")

	count := 0
	for _, def := range plan.Defs {
		if !roundTrippable(def) {
			continue
		}
		fmt.Fprintf(&out, "\t{%s, func() any { return new(acp.%s) }},\n",
			strconv.Quote(def.GoName), def.GoName)
		count++
	}
	out.WriteString("}\n")

	if count == 0 {
		return nil, errors.New("no generated type is round-trippable, which cannot be right")
	}

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("the generated round-trip list does not parse: %w", err)
	}
	return formatted, nil
}

// roundTrippable reports whether a definition names a Go type a caller can
// allocate and hand to the encoder.
func roundTrippable(def *Def) bool {
	if def.GoName != def.Ident {
		// Unexported: the property runs from outside the package, which is where a
		// caller encodes from.
		return false
	}
	switch def.Kind {
	case kindStruct, kindNewtype, kindStringUnion, kindNumberUnion:
		return true
	case kindObjectUnion, kindValueUnion, kindNullArm, kindRawValue:
		return false
	default:
		return false
	}
}
