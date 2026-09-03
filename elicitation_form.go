package acp

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Tangerg/acp/internal/wire"
	"github.com/google/jsonschema-go/jsonschema"
)

// validate keeps the ACP-specific representation at the boundary and delegates
// JSON Schema semantics to the same maintained validator used by the official
// MCP Go SDK. Both peers call it: clients check the answer they are about to send,
// and agents check the answer they received as defense in depth.
func (schema ElicitationSchema) validate(content map[string]ElicitationContentValue) error {
	compiled, err := schema.validationSchema()
	if err != nil {
		return fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	resolved, err := compiled.Resolve(nil)
	if err != nil {
		return fmt.Errorf("acp: invalid elicitation schema: %w", err)
	}
	instance, err := elicitationContentInstance(content)
	if err != nil {
		return fmt.Errorf("acp: invalid form answer: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("acp: form answer does not match requested schema: %w", err)
	}
	return nil
}

func (schema ElicitationSchema) validationSchema() (*jsonschema.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var compiled jsonschema.Schema
	if err := json.Unmarshal(data, &compiled); err != nil {
		return nil, err
	}

	for name, property := range schema.Properties {
		compiledProperty := compiled.Properties[name]
		switch property := property.(type) {
		case *ElicitationPropertySchemaOther:
			// The catch-all exists to preserve a future grammar arm, not to let this
			// version partially interpret that arm using whichever keywords happen
			// to look familiar.
			compiled.Properties[name] = &jsonschema.Schema{}
		case *StringPropertySchema:
			if pattern, ok := property.Pattern.Get(); ok {
				if _, err := regexp.Compile(pattern); err != nil {
					// JSON Schema patterns use the ECMA-262 dialect. The validator uses
					// Go regular expressions, so a valid upstream pattern such as a
					// lookahead must not make the entire elicitation schema invalid.
					compiledProperty.Pattern = ""
				}
			}
		case *MultiSelectPropertySchema:
			if _, unknown := property.Items.(*MultiSelectItemsOther); unknown {
				// Keep array and cardinality checks, but do not guess the value
				// grammar of a future items arm.
				compiledProperty.Items = &jsonschema.Schema{}
			}
		}
	}
	return &compiled, nil
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
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, err
	}
	return instance, nil
}

func (response *CreateElicitationResponse) accepted() bool {
	_, accepted := response.acceptedAction()
	return accepted
}

// acceptedContent distinguishes an accepted form with no content from responses
// whose action does not claim that the user answered the form.
func (response *CreateElicitationResponse) acceptedContent() (
	map[string]ElicitationContentValue,
	bool,
) {
	action, accepted := response.acceptedAction()
	if !accepted {
		return nil, false
	}
	return action.Content.Get()
}

func (response *CreateElicitationResponse) acceptedAction() (*ElicitationAcceptAction, bool) {
	if response == nil {
		return nil, false
	}
	action, accepted := response.Value.(*ElicitationAcceptAction)
	return action, accepted && action != nil
}
