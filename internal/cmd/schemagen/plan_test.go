package main

import (
	"os"
	"path/filepath"
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
