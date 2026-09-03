package acp

import (
	"strings"
	"testing"
)

// The form answer, checked against the form that asked for it.
//
// The cases worth naming are the ones where a validator gets it wrong in a way
// that breaks a working exchange: a whole number answering a form that asked for
// a number, and a constraint this package deliberately does not read.
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
			wants:   "requires",
		},
		{
			name:    "a string where a number was asked for",
			schema:  schema(map[string]ElicitationPropertySchema{"count": &IntegerPropertySchema{}}),
			content: map[string]ElicitationContentValue{"count": text("three")},
			wants:   "asks for integer",
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
			wants:   "asks for integer",
		},
		{
			name: "a number below the form's minimum",
			schema: schema(map[string]ElicitationPropertySchema{
				"count": &IntegerPropertySchema{Minimum: OptValue(int64(1))},
			}),
			content: map[string]ElicitationContentValue{"count": whole(0)},
			wants:   "below the form's minimum",
		},
		{
			name: "a string outside the choices offered",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Enum: OptValue([]string{"main", "next"})},
			}),
			content: map[string]ElicitationContentValue{"branch": text("other")},
			wants:   "not one of the choices",
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
			wants:   "not one of the choices",
		},
		{
			name: "a pattern this package does not read",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^(?=.*x)`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("anything")},
			why: "an ECMA-262 lookahead does not compile as RE2, and refusing an answer " +
				"because this package could not read the pattern would be worse than not reading it",
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
			err := validateElicitationContent(test.schema, test.content)
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
