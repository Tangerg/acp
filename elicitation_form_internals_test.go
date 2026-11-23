package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
	ecmascript "github.com/faustbrian/go-ecma-regexp/v2"
)

func TestFormSchemaIsCompiledBeforeTheHandlerRuns(t *testing.T) {
	called := false
	conn := &ClientConn{client: &Client{config: ClientConfig{Elicitation: &ElicitationHandlers{
		Form: func(context.Context, *ClientConn, *CreateElicitationRequest, *ElicitationFormMode) (*CreateElicitationResponse, error) {
			called = true
			return &CreateElicitationResponse{Value: &CreateElicitationResponseDecline{}}, nil
		},
	}}}}
	params, decodeErr := decodeParams[CreateElicitationRequest](json.RawMessage(
		`{"mode":"form","sessionId":"s","message":"form","requestedSchema":{"type":"object","properties":{"field":{"type":"string","pattern":"["}}}}`))
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	_, err := conn.serveElicitation(t.Context(), &inboundRequest{method: methodElicitationCreate, params: params})
	if called {
		t.Error("an invalid form reached the application handler")
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != ErrorCodeInvalidParams {
		t.Errorf("invalid peer schema returned %v, want invalid params", err)
	}
}

func TestCompiledFormOwnsConstraintsAfterTheWireModelChanges(t *testing.T) {
	property := &StringPropertySchema{Pattern: OptValue(`^main$`), MinLength: OptValue(uint32(4))}
	schema := ElicitationSchema{
		Type:       ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{"branch": property},
	}
	form, err := newCompiledForm(schema)
	if err != nil {
		t.Fatal(err)
	}
	property.Pattern = OptValue(`^x$`)
	property.MinLength = OptValue(uint32(1))
	delete(schema.Properties, "branch")
	valid := ElicitationContentValueString("main")
	if err := form.validate(t.Context(), map[string]ElicitationContentValue{"branch": &valid}); err != nil {
		t.Fatalf("changing the wire model changed compiled constraints: %v", err)
	}
	invalid := ElicitationContentValueString("x")
	if err := form.validate(t.Context(), map[string]ElicitationContentValue{"branch": &invalid}); err == nil {
		t.Fatal("changing the wire model weakened compiled constraints")
	}
}

func TestFormAnswersEnforceDeclaredStringFormats(t *testing.T) {
	for _, test := range []struct {
		format StringFormat
		valid  string
	}{
		{format: StringFormatEmail, valid: "user@example.com"},
		{format: StringFormatURI, valid: "https://example.com/path"},
		{format: StringFormatDate, valid: "2026-11-08"},
		{format: StringFormatDateTime, valid: "2026-11-08T00:00:00Z"},
	} {
		t.Run(string(test.format), func(t *testing.T) {
			form, err := newCompiledForm(ElicitationSchema{
				Type: ElicitationSchemaTypeObject,
				Properties: map[string]ElicitationPropertySchema{
					"value": &StringPropertySchema{Format: OptValue(test.format)},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{test.valid, "not an email, URI, date or time"} {
				value := ElicitationContentValueString(text)
				err := form.validate(t.Context(), map[string]ElicitationContentValue{"value": &value})
				if (err == nil) != (text == test.valid) {
					t.Errorf("format %q, answer %q: %v", test.format, text, err)
				}
			}
		})
	}
}

func TestFormPatternResourceFailureDoesNotBecomeAMismatch(t *testing.T) {
	form, err := newCompiledForm(ElicitationSchema{
		Type: ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{
			"text": &StringPropertySchema{Pattern: OptValue(`^x$|^(a+)+$`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := ElicitationContentValueString(strings.Repeat("a", 32) + "!")
	var limit *ecmascript.LimitError
	var timeout *ecmascript.TimeoutError
	if err := form.validate(t.Context(), map[string]ElicitationContentValue{"text": &value}); !errors.As(err, &limit) && !errors.As(err, &timeout) {
		t.Fatalf("excessive pattern work returned %v, want a resource-limit failure", err)
	}
	value = "x"
	if err := form.validate(t.Context(), map[string]ElicitationContentValue{"text": &value}); err != nil {
		t.Fatalf("a resource failure poisoned the next answer: %v", err)
	}
}

func TestFormPatternsRejectNonECMAScriptSyntax(t *testing.T) {
	for _, pattern := range []string{
		`^(?>a)$`,
		`(?i)^a$`,
		`(?=a)(?>a)`,
		`(?'word'a)`,
		`(?#comment)a`,
		`\q`,
		`a{`,
	} {
		t.Run(pattern, func(t *testing.T) {
			_, err := newCompiledForm(ElicitationSchema{
				Type: ElicitationSchemaTypeObject,
				Properties: map[string]ElicitationPropertySchema{
					"text": &StringPropertySchema{Pattern: OptValue(pattern)},
				},
			})
			if _, ok := errors.AsType[*ecmascript.SyntaxError](err); !ok {
				t.Fatalf("invalid ECMAScript pattern returned %v, want a syntax error", err)
			}
		})
	}
}

func TestFormPatternCompilationHasAResourceBudget(t *testing.T) {
	pattern := strings.Repeat("a", formPatternMaxBytes+1)
	_, err := newCompiledForm(ElicitationSchema{
		Type: ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{
			"text": &StringPropertySchema{Pattern: OptValue(pattern)},
		},
	})
	var limit *ecmascript.LimitError
	if !errors.As(err, &limit) || limit.Kind != ecmascript.LimitPatternBytes {
		t.Fatalf("excessive pattern size returned %v, want a pattern-size limit error", err)
	}
}

func TestFormPatternValidationHonorsCancellation(t *testing.T) {
	form, err := newCompiledForm(ElicitationSchema{
		Type: ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{
			"text": &StringPropertySchema{Pattern: OptValue(`^a$`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	value := ElicitationContentValueString("a")
	if err := form.validate(ctx, map[string]ElicitationContentValue{"text": &value}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled validation returned %v, want context.Canceled", err)
	}
	if err := form.validate(t.Context(), map[string]ElicitationContentValue{"text": &value}); err != nil {
		t.Fatalf("cancellation poisoned the next answer: %v", err)
	}
}

func TestFormURIsFollowJSONSchemaInsteadOfBrowserRepairRules(t *testing.T) {
	form, err := newCompiledForm(ElicitationSchema{
		Type: ElicitationSchemaTypeObject,
		Properties: map[string]ElicitationPropertySchema{
			"link": &StringPropertySchema{Format: OptValue(StringFormatURI)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		text  string
		valid bool
	}{
		{text: "urn:example:note", valid: true},
		{text: "custom:", valid: true},
		{text: "https://example.org/a%20b", valid: true},
		{text: "https://example.org/a b"},
		{text: "https://example.org/界"},
		{text: "https://example.org/?q=%zz"},
		{text: "https://[not-an-ipv6-address]/"},
	} {
		value := ElicitationContentValueString(test.text)
		err := form.validate(t.Context(), map[string]ElicitationContentValue{"link": &value})
		if (err == nil) != test.valid {
			t.Errorf("URI %q valid=%t, error=%v", test.text, test.valid, err)
		}
	}
}

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
			name: "an ECMAScript lookahead that does not match",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^(?=.*x)`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("anything")},
			wants:   "pattern",
		},
		{
			name: "an ECMAScript lookahead that matches",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^(?=.*x)`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("extra")},
		},
		{
			name: "an ECMAScript backreference that matches",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^(.)\1$`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("aa")},
		},
		{
			name: "an ECMAScript surrogate-pair escape",
			schema: schema(map[string]ElicitationPropertySchema{
				"branch": &StringPropertySchema{Pattern: OptValue(`^\uD83D\uDE00$`)},
			}),
			content: map[string]ElicitationContentValue{"branch": text("😀")},
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
			form, err := newCompiledForm(test.schema)
			if err != nil {
				t.Fatalf("compiling form: %v", err)
			}
			err = form.validate(t.Context(), test.content)
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
	form, err := newCompiledForm(schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := form.validate(t.Context(), content); err != nil {
		t.Fatalf("null options constrained the answer: %v", err)
	}
	invalid := ElicitationContentValueInteger(1)
	content["text"] = &invalid
	if err := form.validate(t.Context(), content); err == nil {
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
	form, err := newCompiledForm(schema)
	if err != nil {
		t.Fatal(err)
	}
	err = form.validate(t.Context(), map[string]ElicitationContentValue{"branch": text})
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
