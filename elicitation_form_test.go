package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// The form answer, checked against the form that asked for it.
//
// The cases worth naming are the ones where a representation adapter or regex
// dialect mismatch can make a validator reject a working exchange.
func TestAFormAnswerIsCheckedAgainstItsSchema(t *testing.T) {
	text := func(s string) ElicitationContentValue {
		value := ElicitationContentValueString(s)
		return &value
	}
	whole := func(n int64) ElicitationContentValue {
		value := ElicitationContentValueInteger(n)
		return &value
	}
	fractional := func(f float64) ElicitationContentValue {
		value := ElicitationContentValueNumber(f)
		return &value
	}
	list := func(items ...string) ElicitationContentValue {
		value := ElicitationContentValueStringArray(items)
		return &value
	}

	schema := func(properties map[string]ElicitationPropertySchema, required ...string) ElicitationSchema {
		s := ElicitationSchema{Type: ElicitationSchemaTypeObject, Properties: properties}
		if len(required) > 0 {
			s.Required = OptValue(required)
		}
		return s
	}

	tests := []struct {
		name    string
		schema  ElicitationSchema
		content map[string]ElicitationContentValue
		wants   string
		why     string
	}{
		{
			name:    "a required property left out",
			schema:  schema(map[string]ElicitationPropertySchema{"branch": &StringPropertySchema{}}, "branch"),
			content: map[string]ElicitationContentValue{},
			wants:   "missing property",
		},
		{
			name:    "a string where a number was asked for",
			schema:  schema(map[string]ElicitationPropertySchema{"count": &IntegerPropertySchema{}}),
			content: map[string]ElicitationContentValue{"count": text("three")},
			wants:   "want integer",
		},
		{
			name: "a whole number answering a form that asked for a number",
			schema: schema(map[string]ElicitationPropertySchema{
				"ratio": &NumberPropertySchema{Minimum: OptValue(0.0), Maximum: OptValue(10.0)},
			}),
			content: map[string]ElicitationContentValue{"ratio": whole(3)},
			why: "JSON has one number type, and the integer arm is tried first, so a form " +
				"asking for a number must accept what a whole answer decodes as",
		},
		{
			name: "a fractional answer to a form that asked for an integer",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{},
			}),
			content: map[string]ElicitationContentValue{"count": fractional(3.5)},
			wants:   "want integer",
		},
		{
			name: "a number below the form's minimum",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Minimum: OptValue(int64(1))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(0)},
			wants:   "minimum",
		},
		{
			name: "an integer beyond float precision exceeds the maximum",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Maximum: OptValue(int64(9007199254740992))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(9007199254740993)},
			wants:   "maximum",
		},
		{
			name: "an integer beyond float precision is below the minimum",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Minimum: OptValue(int64(9007199254740993))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(9007199254740992)},
			wants:   "minimum",
		},
		{
			name: "the largest integer stays within its exact bound",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Maximum: OptValue(int64(9223372036854775807))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(9223372036854775807)},
		},
		{
			name: "the smallest integer stays within its exact bound",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Minimum: OptValue(int64(-9223372036854775808))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(-9223372036854775808)},
		},
		{
			name: "a string outside the choices offered",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Enum: OptValue([]string{"main", "next"})},
			}),
			content: map[string]ElicitationContentValue{"branch": text("other")},
			wants:   "one of",
		},
		{
			name: "a string longer than the form allows, counted in characters",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{MaxLength: OptValue(uint32(3))},
			}),
			content: map[string]ElicitationContentValue{"branch": text("ααα")},
			why:     "three code points in six bytes is three characters, which the form allows",
		},
		{
			name: "a multi-select choice the form did not offer",
			schema: schema(map[string]ElicitationPropertySchema{
				"tags": &MultiSelectPropertySchema{
					Items: &StringMultiSelectItems{Enum: []string{"a", "b"}},
				},
			}),
			content: map[string]ElicitationContentValue{"tags": list("a", "c")},
			wants:   "one of",
		},
		{
			name: "an unrepresentable selection minimum cannot wrap around",
			schema: schema(map[string]ElicitationPropertySchema{
				"tags": &MultiSelectPropertySchema{
					MinItems: OptValue(^uint64(0)),
					Items:    &StringMultiSelectItems{Enum: []string{"a"}},
				},
			}),
			content: map[string]ElicitationContentValue{"tags": list("a")},
			wants:   "'not'",
		},
		{
			name: "a selection maximum above the address space admits a short list",
			schema: schema(map[string]ElicitationPropertySchema{
				"tags": &MultiSelectPropertySchema{
					MaxItems: OptValue(^uint64(0)),
					Items:    &StringMultiSelectItems{Enum: []string{"a"}},
				},
			}),
			content: map[string]ElicitationContentValue{"tags": list("a")},
		},
		{
			name: "an unsupported pattern is reported instead of discarded",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^(?=.*x)`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("anything")},
			wants:   "pattern",
		},
		{
			name: "a supported pattern that does not match",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^release/`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("main")},
			wants:   "pattern",
		},
		{
			name: "a property the form does not declare",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{},
			}),
			content: map[string]ElicitationContentValue{"branch": text("main"), "extra": whole(1)},
			why:     "the schema states no additionalProperties, and JSON Schema permits them by default",
		},
		{
			name: "a property kind this package cannot name",
			schema: schema(map[string]ElicitationPropertySchema{
				"colour": &ElicitationPropertySchemaOther{Type: "chromatic"},
			}),
			content: map[string]ElicitationContentValue{"colour": text("warm")},
			why:     "the catch-all arm exists for kinds this package cannot describe a valid value for",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.schema.validate(test.content)
			if test.wants == "" {
				if err != nil {
					t.Fatalf("refused a valid answer: %v (%s)", err, test.why)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted an answer the form does not describe")
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q", err, test.wants)
			}
		})
	}
}

func TestNullFormOptionsMeanNoConstraint(t *testing.T) {
	const encoded = `{
		"type":"object","title":null,"description":null,"required":null,
		"_meta":{"nested":{"pattern":null}},
		"properties":{
			"text":{"type":"string","title":null,"description":null,"minLength":null,"maxLength":null,
				"pattern":null,"format":null,"default":null,"enum":null,"oneOf":null,"_meta":null},
			"count":{"type":"integer","minimum":null,"maximum":null,"default":null},
			"ratio":{"type":"number","minimum":null,"maximum":null,"default":null},
			"enabled":{"type":"boolean","default":null},
			"tags":{"type":"array","minItems":null,"maxItems":null,"default":null,
				"items":{"type":"string","enum":["a","b"],"_meta":null}},
			"titled":{"type":"array","items":{"anyOf":[{"const":"a","title":"A","description":null}]}},
			"choice":{"type":"string","oneOf":[{"const":"a","title":"A","description":null}]},
			"_meta":{"type":"string","pattern":null}
		}
	}`
	var schema ElicitationSchema
	if err := json.Unmarshal([]byte(encoded), &schema); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	content, err := wire.UnmarshalMapFunc([]byte(`{
		"text":"anything","count":1,"ratio":1.5,"enabled":true,
		"tags":["a"],"titled":["a"],"choice":"a","_meta":"text"
	}`), unmarshalElicitationContentValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.validate(content); err != nil {
		t.Fatalf("null options constrained the answer: %v", err)
	}
	invalid := ElicitationContentValueInteger(1)
	content["text"] = &invalid
	if err := schema.validate(content); err == nil {
		t.Fatal("normalizing schema options removed the string type constraint")
	}
	after, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("validation changed the wire schema's null options")
	}
}

func TestAFormAnswerCannotHideANilUnionArm(t *testing.T) {
	var text *ElicitationContentValueString
	schema := ElicitationSchema{
		Type: ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{
			"branch": &StringPropertySchema{},
		},
	}
	err := schema.validate(map[string]ElicitationContentValue{"branch": text})
	if err == nil || !strings.Contains(err.Error(), "nil *ElicitationContentValueString arm") {
		t.Fatalf("validation returned %v, want the typed-nil union error", err)
	}
}

func TestAnObjectUnionCannotHideANilArm(t *testing.T) {
	var accepted *ElicitationAcceptAction
	_, err := json.Marshal(CreateElicitationResponse{Value: accepted})
	if err == nil || !strings.Contains(err.Error(), "nil *ElicitationAcceptAction arm") {
		t.Fatalf("Marshal returned %v, want the typed-nil union error", err)
	}
}
