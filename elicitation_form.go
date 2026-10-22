package acp

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Tangerg/acp/internal/wire"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validate keeps the ACP-specific representation at the boundary and delegates
// JSON Schema semantics to a validator that retains exact numbers on both sides
// of a bound. Both peers use this boundary for form answers.
func (e ElicitationSchema) validate(content map[string]ElicitationContentValue) error {
	compiled, err := e.validationSchema()
	if err != nil {
		return fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const resource = "urn:acp:elicitation"
	if addErr := compiler.AddResource(resource, compiled); addErr != nil {
		return fmt.Errorf("acp: invalid elicitation schema: %w", addErr)
	}
	resolved, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	instance, err := elicitationContentInstance(content)
	if err != nil {
		return fmt.Errorf("acp: invalid form answer: %w", err)
	}
	if invalid := resolved.Validate(instance); invalid != nil {
		return fmt.Errorf("acp: form answer does not match requested schema: %w", invalid)
	}
	return nil
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
	properties, ok := compiled["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("elicitation properties encoded as %T", compiled["properties"])
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
				compiledProperty["items"] = true
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
	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		bound, isNumber := schema[key].(json.Number)
		if !isNumber {
			continue
		}
		if count, err := bound.Int64(); err == nil && count <= int64(^uint(0)>>1) {
			continue
		}
		delete(schema, key)
		if key == "minLength" || key == "minItems" {
			schema["not"] = map[string]any{}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, property := range properties {
		normalizeFormSchema(property)
	}
	normalizeFormSchema(schema["items"])
	for _, key := range []string{"oneOf", "anyOf"} {
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

// acceptedContent distinguishes an accepted form with no content from responses
// whose action does not claim that the user answered the form.
func (c *CreateElicitationResponse) acceptedContent() (
	map[string]ElicitationContentValue,
	bool,
) {
	action, accepted := c.acceptedAction()
	if !accepted {
		return nil, false
	}
	return action.Content.Get()
}

func (c *CreateElicitationResponse) acceptedAction() (*ElicitationAcceptAction, bool) {
	if c == nil {
		return nil, false
	}
	action, accepted := c.Value.(*ElicitationAcceptAction)
	return action, accepted && action != nil
}
