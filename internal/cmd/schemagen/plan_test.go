package main

import (
	"encoding/json"
	"net/url"
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
// params. Package jsonrpc owns it, so generating a second set of types for it
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
		{
			name:   "an int64 minimum adjacent to its native bound",
			schema: `{"type":"integer","format":"int64","minimum":-9223372036854775807}`,
			wants:  "is not the low bound of",
			why:    "float rounding must not erase the schema's narrower range",
		},
		{
			name:   "a uint64 maximum adjacent to its native bound",
			schema: `{"type":"integer","format":"uint64","maximum":18446744073709551614}`,
			wants:  "is not the high bound of",
			why:    "the Go type permits one more value than this schema",
		},
		{
			name:   "a format that belongs to another JSON type",
			schema: `{"type":"integer","format":"double"}`,
			wants:  "belongs to number",
			why:    "an integer decoded as float64 would accept 1.5",
		},
		{
			name:   "a bounded floating point value",
			schema: `{"type":"number","format":"double","minimum":0}`,
			wants:  "a bound on float64 is not implemented",
			why:    "float64 supplies no bound the schema's minimum could be equal to",
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

func TestNativeIntegerBoundsRemainExact(t *testing.T) {
	planner := newPlanner(loadSchema(t))
	for _, definition := range []string{
		`{"type":"integer","format":"int64","minimum":-9223372036854775808,"maximum":9223372036854775807}`,
		`{"type":"integer","format":"uint64","minimum":0,"maximum":18446744073709551615}`,
		`{"type":"integer","format":"int64","minimum":-9.223372036854775808e18,"maximum":9.223372036854775807e18}`,
	} {
		var schema Schema
		if err := schema.UnmarshalJSON([]byte(definition)); err != nil {
			t.Fatal(err)
		}
		if _, err := planner.valueType(&schema); err != nil {
			t.Fatalf("native bounds rejected for %s: %v", definition, err)
		}
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
		`"a":{"type":"string","maxLength":3}}}}}`
	err := checkKnownKeywords([]byte(withUnknown))
	if err == nil {
		t.Fatal("an unimplemented keyword was accepted")
	}
	if !strings.Contains(err.Error(), "maxLength") {
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
		{
			name: "integer default just beyond an exact maximum",
			doc:  `{"$defs":{"Thing":{"type":"integer","maximum":9007199254740992,"default":9007199254740993}}}`,
		},
		{
			name: "nested default with an escaped property name",
			doc:  `{"$defs":{"Thing":{"type":"object","properties":{"~/%":{"type":"integer","minimum":9007199254740993,"default":9007199254740992}}}}}`,
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

func TestLoadDocumentChecksEscapedSchemaLocations(t *testing.T) {
	const document = `{"$defs":{"Thing":{"type":"object","properties":{"~/%":{"type":"integer","default":9007199254740993}}}}}`
	if _, err := loadDocument([]byte(document)); err != nil {
		t.Fatalf("valid default at an escaped location rejected: %v", err)
	}
}

func TestLoadDocumentDoesNotLoadExternalReferences(t *testing.T) {
	file := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(file, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	location := (&url.URL{Scheme: "file", Path: filepath.ToSlash(file)}).String()
	document, err := json.Marshal(map[string]any{
		"$defs": map[string]any{"Thing": map[string]string{"$ref": location}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadDocument(document); err == nil || !strings.Contains(err.Error(), "no URLLoader registered") {
		t.Fatalf("external reference returned %v, want loading disabled", err)
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
	def := &Def{GoName: "Other", Body: &structBody{Fields: []*Field{{JSONName: "mode"}}}}

	t.Run("the elicitation scopes", func(t *testing.T) {
		var arm Schema
		if err := arm.UnmarshalJSON([]byte(`{"anyOf":[
			{"allOf":[{"$ref":"#/$defs/ElicitationSessionScope"}]},
			{"allOf":[{"$ref":"#/$defs/ElicitationRequestScope"}]}
		]}`)); err != nil {
			t.Fatalf("the case's own schema does not parse: %v", err)
		}
		groups, err := planner.catchAllRequiredGroups(&arm, def.structure())
		if err != nil {
			t.Fatalf("catchAllRequiredGroups: %v", err)
		}
		want := []catchAllAlternative{
			{GoName: "ElicitationSessionScope", Required: []string{"sessionId"}},
			{GoName: "ElicitationRequestScope", Required: []string{"requestId"}},
		}
		if !reflect.DeepEqual(groups, want) {
			t.Fatalf("read %v, want %v", groups, want)
		}
	})

	t.Run("an arm that is not a union", func(t *testing.T) {
		var arm Schema
		if err := arm.UnmarshalJSON([]byte(`{"type":"object"}`)); err != nil {
			t.Fatalf("the case's own schema does not parse: %v", err)
		}
		groups, err := planner.catchAllRequiredGroups(&arm, def.structure())
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
			declaring := &Def{GoName: "Other", Body: &structBody{Fields: []*Field{{JSONName: "elicitationId"}}}}
			_, err := planner.catchAllRequiredGroups(&arm, declaring.structure())
			if err == nil {
				t.Fatal("the shape was accepted; if it is implemented now, move this case above")
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q", err, test.wants)
			}
		})
	}
}

// A known keyword is still meaningless where no planning step consumes it, and
// accepting it would produce a type that silently does not enforce it.
func TestKeywordsAreRefusedWhereTheyAreNotConsumed(t *testing.T) {
	planner := newPlanner(loadSchema(t))
	if err := planner.allocate(); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	for name, test := range map[string]struct{ schema, wants string }{
		"a constant property":              {`{"type":"string","const":"x"}`, "const on a property"},
		"a negated property":               {`{"type":"string","not":{"type":"integer"}}`, "not on a property"},
		"unevaluatedProperties on a field": {`{"type":"string","unevaluatedProperties":false}`, "unevaluatedProperties"},
	} {
		t.Run(name, func(t *testing.T) {
			var schema Schema
			if err := schema.UnmarshalJSON([]byte(test.schema)); err != nil {
				t.Fatal(err)
			}
			_, err := planner.planField("a", &schema, true)
			if err == nil || !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("planField(%s) = %v, want a refusal mentioning %q", test.schema, err, test.wants)
			}
		})
	}

	var closed Schema
	if err := closed.UnmarshalJSON([]byte(
		`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := planner.planStruct(&Def{}, &closed, ""); err == nil ||
		!strings.Contains(err.Error(), "additionalProperties") {
		t.Fatalf("a closed struct was accepted: %v", err)
	}
}

func planSchema(t *testing.T, schema string, manifest Manifest) (*Plan, error) {
	t.Helper()
	doc, err := loadDocument([]byte(schema))
	if err != nil {
		t.Fatalf("the case's own schema does not load: %v", err)
	}
	return newPlanner(doc).Plan(&manifest, "test")
}

// The scope a generated header reports is what was planned, including a
// definition reached only through a map's values.
func TestTheClosureIsWhatWasPlanned(t *testing.T) {
	plan, err := planSchema(t, `{"$defs":{
		"A":{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"$ref":"#/$defs/B"}}}},
		"B":{"type":"string"},
		"C":{"type":"string"}
	}}`, Manifest{Payloads: []string{"A"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "B"}; !reflect.DeepEqual(plan.Closure, want) {
		t.Fatalf("closure = %v, want %v", plan.Closure, want)
	}
}

func TestAReachableProbeStopsGeneration(t *testing.T) {
	_, err := planSchema(t, `{"$defs":{
		"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},
		"B":{"type":"string"}
	}}`, Manifest{Payloads: []string{"A"}, Probes: map[string]string{"B": "a string newtype"}})
	if err == nil || !strings.Contains(err.Error(), "probe B is reachable") {
		t.Fatalf("Plan = %v, want the reachable probe refused", err)
	}
}

// An arm's tag is the constant of the union's discriminant property, whatever
// other constants the arm carries, so generation is deterministic.
func TestAnArmsTagIsItsDiscriminantsConstant(t *testing.T) {
	var union Schema
	if err := union.UnmarshalJSON([]byte(`{"anyOf":[
		{"type":"object","properties":{"kind":{"type":"string","const":"a"}}},
		{"type":"object","properties":{"kind":{"type":"string","const":"b"},"version":{"type":"string","const":"v1"}}}
	]}`)); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		disc := discriminant(&union, union.Arms())
		tag, err := union.Arms()[1].armTag(disc)
		if err != nil || disc != "kind" || tag != "b" {
			t.Fatalf("discriminant %q, tag %q, err %v; want kind and b", disc, tag, err)
		}
	}
}

// The reserved values are the `not` clause's own, and a clause that disagrees
// with the known arms is refused rather than silently replaced by them.
func TestACatchAllReservesWhatItsNotClauseSays(t *testing.T) {
	_, err := planSchema(t, `{"$defs":{
		"U":{"anyOf":[
			{"type":"object","properties":{"mode":{"type":"string","const":"form"}},"required":["mode"]},
			{"title":"Other","type":"object","properties":{"mode":{"type":"string"}},"required":["mode"],
			 "not":{"anyOf":[{"type":"object","properties":{"mode":{"type":"string","const":"zzz"}},"required":["mode"]}]}}
		]}
	}}`, Manifest{Payloads: []string{"U"}})
	if err == nil || !strings.Contains(err.Error(), "reserves [zzz]") {
		t.Fatalf("Plan = %v, want the disagreeing not clause refused", err)
	}
}

func TestAnExportedTypeCannotReachAnUnexportedOneThroughAContainer(t *testing.T) {
	_, err := planSchema(t, `{"$defs":{
		"Pub":{"type":"object","properties":{"ids":{"type":"array","items":{"$ref":"#/$defs/Hidden"}}}},
		"Hidden":{"type":"string"}
	}}`, Manifest{Payloads: []string{"Pub"}, Internal: map[string]string{"Hidden": "internal"}})
	if err == nil || !strings.Contains(err.Error(), "reaches the unexported hidden") {
		t.Fatalf("Plan = %v, want the hidden element type refused", err)
	}
}

// Imports come from the selectors the code uses, not from words in a comment.
func TestImportsIgnorePackageNamesInComments(t *testing.T) {
	source, err := goFile("// header\n", []byte("// Use slices. Then fmt.\ntype T struct{}\n\nvar _ = errors.New\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), `"slices"`) || strings.Contains(string(source), `"fmt"`) ||
		!strings.Contains(string(source), `"errors"`) {
		t.Fatalf("imports were not derived from selectors:\n%s", source)
	}
}

func TestMethodShapesComeFromTheRoutingUnions(t *testing.T) {
	doc := loadSchema(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schema", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	table, err := loadMethodTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	methods, err := planMethods(doc, table)
	if err != nil {
		t.Fatal(err)
	}
	shapes := make(map[string]methodShape, len(methods))
	scoped := make(map[string]bool, len(methods))
	for _, method := range methods {
		shapes[method.Name] = method.Shape
		scoped[method.Name] = method.RequiresSessionID
	}
	for name, want := range map[string]methodShape{
		"session/prompt":   shapeRequest,
		"session/cancel":   shapeNotification,
		"session/update":   shapeNotification,
		"$/cancel_request": shapeNotification,
		"terminal/kill":    shapeRequest,
	} {
		if shapes[name] != want {
			t.Errorf("%s has shape %d, want %d", name, shapes[name], want)
		}
	}
	if !scoped["session/prompt"] || scoped["session/new"] || scoped["initialize"] {
		t.Errorf("session scoping misread: prompt %t, new %t, initialize %t",
			scoped["session/prompt"], scoped["session/new"], scoped["initialize"])
	}
}
