package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/Tangerg/acp/internal/wire"
	ecmascript "github.com/faustbrian/go-ecma-regexp/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A compiled form owns the constraints before a handler can edit its wire model.
// Pattern matching stays separate because the validator's boolean regexp adapter
// cannot distinguish a mismatch from a resource-limit failure.
type compiledForm struct {
	schema   *jsonschema.Schema
	patterns []formPattern
}

type formPattern struct {
	property   string
	expression *ecmascript.JSONSchemaPattern
}

// Peer-controlled interactive forms need smaller compile and execution budgets
// than general-purpose pattern processing. The wall-time check is cooperative.
const (
	formPatternMaxBytes        = 64 << 10
	formPatternMaxNodes        = 16 << 10
	formPatternMaxDepth        = 64
	formPatternMaxCaptures     = 128
	formPatternMaxClasses      = 256
	formPatternMaxInstructions = 64 << 10
	formPatternMaxSteps        = 1_000_000
	formPatternMaxBacktracks   = 100_000
	formPatternMaxStackDepth   = 4096
	formPatternMaxAllocations  = 200_000
	formPatternTimeout         = 100 * time.Millisecond
)

func newCompiledForm(schema ElicitationSchema) (*compiledForm, error) {
	compiled, err := schema.validationSchema()
	if err != nil {
		return nil, fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	patterns, err := compileFormPatterns(schema, compiled)
	if err != nil {
		return nil, fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{
		Name: string(StringFormatURI),
		Validate: func(value any) error {
			if text, isString := value.(string); isString {
				return wire.ValidateURI(text)
			}
			return nil
		},
	})
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const resource = "urn:acp:elicitation"
	if addErr := compiler.AddResource(resource, compiled); addErr != nil {
		return nil, fmt.Errorf("acp: invalid elicitation schema: %w", addErr)
	}
	resolved, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	return &compiledForm{schema: resolved, patterns: patterns}, nil
}

// validateResponse holds an accepting answer to the form. An accept without
// content is an empty answer, which a schema with required properties refuses.
// A nil form has nothing to hold an answer to.
func (f *compiledForm) validateResponse(ctx context.Context, response *CreateElicitationResponse) error {
	action, accepted := response.acceptedAction()
	if f == nil || !accepted {
		return nil
	}
	content, _ := action.Content.Get()
	if content == nil {
		content = map[string]ElicitationContentValue{}
	}
	return f.validate(ctx, content)
}

func (f *compiledForm) validate(ctx context.Context, content map[string]ElicitationContentValue) error {
	instance, err := elicitationContentInstance(content)
	if err != nil {
		return fmt.Errorf("acp: invalid form answer: %w", err)
	}
	if invalid := f.schema.Validate(instance); invalid != nil {
		return fmt.Errorf("acp: form answer does not match requested schema: %w", invalid)
	}
	options := ecmascript.DefaultMatchOptions()
	options.Limits.Steps = formPatternMaxSteps
	options.Limits.Backtracks = formPatternMaxBacktracks
	options.Limits.StackDepth = formPatternMaxStackDepth
	options.Limits.RecursionDepth = formPatternMaxDepth
	options.Limits.Allocations = formPatternMaxAllocations
	options.Limits.WallTime = formPatternTimeout
	for _, pattern := range f.patterns {
		value, present := content[pattern.property].(*ElicitationContentValueString)
		if !present {
			continue
		}
		matched, matchErr := pattern.expression.Match(ctx, string(*value), options)
		if matchErr != nil {
			return fmt.Errorf("acp: evaluating form pattern for %q: %w", pattern.property, matchErr)
		}
		if !matched {
			return fmt.Errorf("acp: form answer %q does not match its declared pattern", pattern.property)
		}
	}
	return nil
}

// The JSON Schema keywords the private projection rewrites.
const (
	keywordProperties = "properties"
	keywordPattern    = "pattern"
	keywordItems      = "items"
	keywordNot        = "not"
	keywordMinLength  = "minLength"
	keywordMaxLength  = "maxLength"
	keywordMinItems   = "minItems"
	keywordMaxItems   = "maxItems"
	keywordOneOf      = "oneOf"
	keywordAnyOf      = "anyOf"
)

func compileFormPatterns(schema ElicitationSchema, projection map[string]any) ([]formPattern, error) {
	properties, _ := projection[keywordProperties].(map[string]any)
	options := ecmascript.DefaultCompileOptions()
	options.Parse.Limits = ecmascript.ParseLimits{
		PatternBytes:     formPatternMaxBytes,
		ASTDepth:         formPatternMaxDepth,
		Captures:         formPatternMaxCaptures,
		CharacterClasses: formPatternMaxClasses,
		ASTNodes:         formPatternMaxNodes,
	}
	options.Limits.ProgramInstructions = formPatternMaxInstructions
	var patterns []formPattern
	for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
		property, isString := schema.Properties[name].(*StringPropertySchema)
		if !isString {
			continue
		}
		pattern, present := property.Pattern.Get()
		if !present {
			continue
		}
		expression, err := ecmascript.CompileJSONSchemaPattern(pattern, options)
		if err != nil {
			return nil, fmt.Errorf("property %q pattern: %w", name, err)
		}
		patterns = append(patterns, formPattern{property: name, expression: expression})
		object, _ := properties[name].(map[string]any)
		delete(object, keywordPattern)
	}
	return patterns, nil
}

func (e ElicitationSchema) validationSchema() (map[string]any, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var compiled map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decodeErr := decoder.Decode(&compiled); decodeErr != nil {
		return nil, decodeErr
	}
	properties, ok := compiled[keywordProperties].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("elicitation properties encoded as %T", compiled[keywordProperties])
	}

	for name, property := range e.Properties {
		switch property := property.(type) {
		case *ElicitationPropertySchemaOther:
			// The catch-all exists to preserve a future grammar arm, not to let this
			// version partially interpret that arm using whichever keywords happen
			// to look familiar.
			properties[name] = true
		case *MultiSelectPropertySchema:
			if _, unknown := property.Items.(*MultiSelectItemsOther); unknown {
				// Keep array and cardinality checks, but do not guess the value
				// grammar of a future items arm.
				compiledProperty, isObject := properties[name].(map[string]any)
				if !isObject {
					return nil, fmt.Errorf("elicitation property %q encoded as %T", name, properties[name])
				}
				compiledProperty[keywordItems] = true
			}
		}
	}
	normalizeFormSchema(compiled)
	return compiled, nil
}

// Only this private projection is normalized; metadata and the wire model keep
// their original values. ACP uses null and omission for the same absent option,
// whereas JSON Schema requires omission.
func normalizeFormSchema(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, value := range schema {
		if value == nil {
			delete(schema, key)
		}
	}
	// Go strings and slices cannot exceed MaxInt. A larger maximum adds no
	// constraint; a larger minimum is unsatisfiable. Compile those meanings
	// before the validator narrows cardinalities to int.
	for _, key := range []string{keywordMinLength, keywordMaxLength, keywordMinItems, keywordMaxItems} {
		bound, isNumber := schema[key].(json.Number)
		if !isNumber {
			continue
		}
		if count, err := bound.Int64(); err == nil && count <= math.MaxInt {
			continue
		}
		delete(schema, key)
		if key == keywordMinLength || key == keywordMinItems {
			schema[keywordNot] = map[string]any{}
		}
	}
	properties, _ := schema[keywordProperties].(map[string]any)
	for _, property := range properties {
		normalizeFormSchema(property)
	}
	normalizeFormSchema(schema[keywordItems])
	for _, key := range []string{keywordOneOf, keywordAnyOf} {
		options, _ := schema[key].([]any)
		for _, option := range options {
			normalizeFormSchema(option)
		}
	}
}

// MarshalMapFunc is important here: marshaling the interface-valued map directly
// would encode a typed nil pointer as JSON null, bypassing the generated union's
// invariant that every selected arm contains a value. Decoding the result also
// gives the validator ordinary JSON values instead of the wire union wrappers.
func elicitationContentInstance(content map[string]ElicitationContentValue) (any, error) {
	data, err := wire.MarshalMapFunc(content, marshalElicitationContentValue)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

func (c *CreateElicitationResponse) accepted() bool {
	_, accepted := c.acceptedAction()
	return accepted
}

func (c *CreateElicitationResponse) acceptedAction() (*ElicitationAcceptAction, bool) {
	if c == nil {
		return nil, false
	}
	action, accepted := c.Value.(*ElicitationAcceptAction)
	return action, accepted && action != nil
}
