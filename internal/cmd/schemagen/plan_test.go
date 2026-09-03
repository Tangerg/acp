package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadSchema(t *testing.T) *Document {
	t.Helper()
	path := filepath.Join("..", "..", "..", "schema", "schema.json")
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed path inside this repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := loadDocument(raw)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return doc
}

// The generator plans every definition in the schema except the JSON-RPC
// envelopes, and the envelopes are excluded on purpose rather than for want of
// implementation.
//
// The envelope is JSON-RPC's grammar, not ACP's: an id, a method name, and
// params. internal/jsonrpc2 owns it, so generating a second set of types for it
// would be two sources of truth for one thing — and the routing unions inside it,
// "every request an agent can send", are of no use to a connection that
// dispatches on the method name.
//
// The list is the boundary, and it is checked in both directions: a definition
// that starts failing, and one that stops.
func TestEverythingButTheEnvelopesCanBePlanned(t *testing.T) {
	excluded := map[string]string{
		"AgentRequest":       "a JSON-RPC request envelope, whose params is a routing union of every agent method's",
		"AgentResponse":      "a JSON-RPC response envelope, either a result or an error",
		"AgentNotification":  "a JSON-RPC notification envelope",
		"ClientRequest":      "the same, in the other direction",
		"ClientResponse":     "the same, in the other direction",
		"ClientNotification": "the same, in the other direction",
	}

	doc := loadSchema(t)
	for _, name := range sortedKeys(doc.Defs) {
		_, err := newPlannerFor(t, doc).plan(name)
		reason, isExcluded := excluded[name]
		switch {
		case err != nil && !isExcluded:
			t.Errorf("%s cannot be planned: %v", name, err)
		case err == nil && isExcluded:
			t.Errorf("%s can now be planned, but is excluded as %q; decide which is right", name, reason)
		}
	}

	for name := range excluded {
		if _, defined := doc.Defs[name]; !defined {
			t.Errorf("the exclusion list names %s, which the schema does not define", name)
		}
	}
}

func newPlannerFor(t *testing.T, doc *Document) *planner {
	t.Helper()
	planner := newPlanner(doc)
	if err := planner.allocate(); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	return planner
}

// The property-level refusals, stated on constructed schemas rather than on real
// definitions: every definition that reaches one of these is stopped by a
// definition-level refusal first, so the table above cannot reach them.
func TestThePlannerRefusesPropertyShapesItDoesNotImplement(t *testing.T) {
	planner := newPlanner(loadSchema(t))
	if err := planner.allocate(); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	tests := []struct {
		name   string
		schema string
		wants  string
		why    string
	}{
		{
			name:   "an inline object",
			schema: `{"type":"object","properties":{"a":{"type":"string"}}}`,
			wants:  "an inline object is not implemented",
			why:    "the schema names its object types, and one that does not has no Go type to be",
		},
		{
			name:   "an object with neither properties nor additionalProperties",
			schema: `{"type":"object","required":["a"]}`,
			wants:  "an inline object is not implemented",
			why:    "there is nothing to generate: the schema names its object types",
		},
		{
			name:   "an array with no items",
			schema: `{"type":"array"}`,
			wants:  "an array with no items schema",
			why:    "there is no element type to generate",
		},
		{
			name:   "an integer with no format",
			schema: `{"type":"integer"}`,
			wants:  "unimplemented format",
			why:    "the Go type comes from the format, and the bounds come with it",
		},
		{
			name:   "a bound the Go type does not already enforce",
			schema: `{"type":"integer","format":"int64","minimum":1}`,
			wants:  "is not the low bound of",
			why: "every bound in the schema is one the chosen Go type gives for free, and the " +
				"generator asserts that rather than assuming it",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var schema Schema
			if err := schema.UnmarshalJSON([]byte(test.schema)); err != nil {
				t.Fatalf("the case's own schema does not parse: %v", err)
			}
			_, err := planner.valueType(&schema)
			if err == nil {
				t.Fatalf("accepted %s; if it is now implemented, delete this case (%s)", test.schema, test.why)
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q (%s)", err, test.wants, test.why)
			}
		})
	}
}

// The names every definition and arm in the schema would get are allocated up
// front, whether the manifest reaches them or not. If that pass cannot complete,
// growing the manifest would rename something already published — so it is
// checked against the whole schema rather than against the current closure.
func TestEveryNameInTheSchemaCanBeAllocated(t *testing.T) {
	if err := newPlanner(loadSchema(t)).allocate(); err != nil {
		t.Fatalf("allocating names over the whole schema failed: %v", err)
	}
}

// A keyword the generator does not implement has to stop generation. Otherwise a
// schema bump that adds a constraint silently produces types that do not enforce
// it, which is the difference between a generator that is behind and one that is
// wrong.
func TestUnimplementedKeywordsAreReported(t *testing.T) {
	const withUnknown = `{"$defs":{"Thing":{"type":"object","properties":{` +
		`"a":{"type":"string","minLength":3}}}}}`
	err := checkKnownKeywords([]byte(withUnknown))
	if err == nil {
		t.Fatal("an unimplemented keyword was accepted")
	}
	if !strings.Contains(err.Error(), "minLength") {
		t.Fatalf("the failure %q does not name the keyword", err)
	}
	if !strings.Contains(err.Error(), "#/$defs/Thing/properties/a") {
		t.Fatalf("the failure %q does not name where the keyword was", err)
	}

	// And the vendored schema itself uses nothing the generator has not seen,
	// which is what makes the refusals above the only boundary there is.
	path := filepath.Join("..", "..", "..", "schema", "schema.json")
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed path inside this repository.
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := checkKnownKeywords(raw); err != nil {
		t.Fatalf("the vendored schema uses a keyword the generator does not implement: %v", err)
	}
}

// The local projection below deliberately knows only the ACP generator's
// vocabulary. These cases prove that narrowing the projection does not also make
// it the authority on the JSON Schema standard.
func TestLoadDocumentRejectsInvalidJSONSchema(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "unresolved reference",
			doc:  `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"Thing":{"$ref":"#/$defs/Missing"}}}`,
		},
		{
			name: "default outside its schema",
			doc:  `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"Thing":{"type":"string","default":42}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadDocument([]byte(test.doc)); err == nil {
				t.Fatalf("loadDocument accepted %s", test.doc)
			}
		})
	}
}

// Property order decides the generated field order and the encoded key order, so
// a message can be read against the schema that defines it. Go maps have no order
// and the schema document does, so this is read out of the raw bytes.
func TestPropertyOrderIsTheDocumentsOwn(t *testing.T) {
	doc := loadSchema(t)
	prompt := doc.Defs["PromptRequest"]
	want := []string{"sessionId", "prompt", "_meta"}

	if len(prompt.PropertyOrder) != len(want) {
		t.Fatalf("PromptRequest has properties %v, want %v", prompt.PropertyOrder, want)
	}
	for i := range want {
		if prompt.PropertyOrder[i] != want[i] {
			t.Fatalf("property %d is %q, want %q", i, prompt.PropertyOrder[i], want[i])
		}
	}
}

// A union has to reach the generated function that encodes and decodes it. The
// emitter routes one at the property itself, one element deep inside an array,
// and one value deep inside an object; anything deeper falls through to
// encoding/json, which cannot write the discriminant the union owns and cannot
// decode into an interface at all.
//
// The failure that would produce is not a build error. It is a message encoded
// without its discriminant, put on the wire, and unreadable by the peer and by
// this package alike — which is what the map form of it did until a schema
// property first had one. So the planner refuses the shape instead.
func TestAUnionMustReachItsGeneratedCodec(t *testing.T) {
	union := &ValueType{Kind: vRef, Go: "ContentBlock", Ident: "ContentBlock", IsUnion: true}
	scalar := &ValueType{Kind: vScalar, Go: "string"}

	slice := func(elem *ValueType) *ValueType {
		return &ValueType{Kind: vSlice, Go: "[]" + elem.Go, Elem: elem}
	}
	object := func(elem *ValueType) *ValueType {
		return &ValueType{Kind: vMap, Go: "map[string]" + elem.Go, Elem: elem}
	}

	routed := map[string]*ValueType{
		"the property itself":      union,
		"one element into a list":  slice(union),
		"one value into an object": object(union),
		"a list of scalars":        slice(scalar),
		"an object of scalars":     object(scalar),
	}
	for name, value := range routed {
		t.Run(name, func(t *testing.T) {
			if err := checkUnionIsRouted(value); err != nil {
				t.Fatalf("refused %s, which the emitter routes: %v", value.Go, err)
			}
		})
	}

	unrouted := map[string]*ValueType{
		"a list of lists":      slice(slice(union)),
		"a list of objects":    slice(object(union)),
		"an object of lists":   object(slice(union)),
		"an object of objects": object(object(union)),
	}
	for name, value := range unrouted {
		t.Run(name, func(t *testing.T) {
			err := checkUnionIsRouted(value)
			if err == nil {
				t.Fatalf("accepted %s; if the emitter routes it now, move this case above", value.Go)
			}
			if !strings.Contains(err.Error(), "no generated codec") {
				t.Fatalf("refused with %q, which does not say the codec is missing", err)
			}
		})
	}
}

// A catch-all arm may itself be a union, and reading its alternatives is only
// possible for the shapes below. Anything else stops generation rather than being
// dropped, which is what the elicitation modes were: the arm carried a scope union
// the generator did not see, so this package accepted messages the published
// schema rejects.
func TestACatchAllArmsAlternativesAreReadOrRefused(t *testing.T) {
	planner := newPlanner(loadSchema(t))
	if err := planner.allocate(); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// The arm declares its discriminant and nothing else, which is what a
	// catch-all is.
	def := &Def{GoName: "Other", Fields: []*Field{{JSONName: "mode"}}}

	t.Run("the elicitation scopes", func(t *testing.T) {
		var arm Schema
		if err := arm.UnmarshalJSON([]byte(`{"anyOf":[
			{"allOf":[{"$ref":"#/$defs/ElicitationSessionScope"}]},
			{"allOf":[{"$ref":"#/$defs/ElicitationRequestScope"}]}
		]}`)); err != nil {
			t.Fatalf("the case's own schema does not parse: %v", err)
		}
		groups, err := planner.catchAllRequiredGroups(&arm, def)
		if err != nil {
			t.Fatalf("catchAllRequiredGroups: %v", err)
		}
		want := [][]string{{"sessionId"}, {"requestId"}}
		if !reflect.DeepEqual(groups, want) {
			t.Fatalf("read %v, want %v", groups, want)
		}
	})

	t.Run("an arm that is not a union", func(t *testing.T) {
		var arm Schema
		if err := arm.UnmarshalJSON([]byte(`{"type":"object"}`)); err != nil {
			t.Fatalf("the case's own schema does not parse: %v", err)
		}
		groups, err := planner.catchAllRequiredGroups(&arm, def)
		if err != nil || groups != nil {
			t.Fatalf("read %v, %v; an arm with no alternatives constrains nothing", groups, err)
		}
	})

	refusals := map[string]struct{ schema, wants string }{
		"an alternative that is not a single $ref": {
			schema: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`,
			wants:  "not a single $ref",
		},
		"an alternative requiring nothing": {
			schema: `{"anyOf":[{"allOf":[{"$ref":"#/$defs/ElicitationAcceptAction"}]}]}`,
			wants:  "requires no property",
		},
		"an alternative requiring a property the arm declares": {
			schema: `{"anyOf":[{"allOf":[{"$ref":"#/$defs/CompleteElicitationNotification"}]}]}`,
			wants:  "which the arm declares itself",
		},
	}
	for name, test := range refusals {
		t.Run(name, func(t *testing.T) {
			var arm Schema
			if err := arm.UnmarshalJSON([]byte(test.schema)); err != nil {
				t.Fatalf("the case's own schema does not parse: %v", err)
			}
			declaring := &Def{GoName: "Other", Fields: []*Field{{JSONName: "elicitationId"}}}
			_, err := planner.catchAllRequiredGroups(&arm, declaring)
			if err == nil {
				t.Fatal("the shape was accepted; if it is implemented now, move this case above")
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q", err, test.wants)
			}
		})
	}
}
