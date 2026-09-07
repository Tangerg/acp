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
func (e ElicitationSchema) validate(content map[string]ElicitationContentValue) error {
	compiled, err := e.validationSchema()
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
	if invalid := resolved.Validate(instance); invalid != nil {
		return fmt.Errorf("acp: form answer does not match requested schema: %w", invalid)
	}
	return nil
}

func (e ElicitationSchema) validationSchema() (*jsonschema.Schema, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var compiled jsonschema.Schema
	if decodeErr := json.Unmarshal(data, &compiled); decodeErr != nil {
		return nil, decodeErr
	}

	for name, property := range e.Properties {
		compiledProperty := compiled.Properties[name]
		switch property := property.(type) {
		case *ElicitationPropertySchemaOther:
			// The catch-all exists to preserve a future grammar arm, not to let this
			// version partially interpret that arm using whichever keywords happen
			// to look familiar.
			compiled.Properties[name] = &jsonschema.Schema{}
		case *StringPropertySchema:
			if pattern, ok := property.Pattern.Get(); ok {
				if _, unsupported := regexp.Compile(pattern); unsupported != nil {
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
	if decodeErr := json.Unmarshal(data, &instance); decodeErr != nil {
		return nil, decodeErr
	}
	return instance, nil
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
