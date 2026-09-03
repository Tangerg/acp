package acp

import (
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// A form elicitation names the shape of its answer, and both sides are told to
// check it: "Clients SHOULD validate user input against the provided JSON Schema
// before sending" and "Agents SHOULD also validate received data matches the
// requested schema, as defense-in-depth".
//
// Neither is a MUST, and this package keeps both anyway because neither costs a
// round trip. A client that would send an answer its own form does not describe
// learns so from its own handler's return; an agent that receives one learns
// before the value reaches the code that asked for it.
//
// # What it does not check
//
// `pattern` and `format` are skipped, and skipped on purpose. A JSON Schema
// pattern is an ECMA-262 regular expression, Go's regexp is RE2, and the two
// disagree on constructs a real form might use — a lookahead compiles in one and
// not the other. Rejecting an answer because this package could not read the
// pattern would be worse than not checking it. `format` has the same shape of
// problem with less to gain.
//
// A property whose schema is the catch-all arm is skipped for the reason the arm
// exists: it is a kind of property this package cannot name, so it cannot say what
// a valid value for one looks like.
//
// A property the schema does not declare is left alone. The schema has no
// `additionalProperties`, and JSON Schema's default is to permit them.
func validateElicitationContent(
	schema ElicitationSchema,
	content map[string]ElicitationContentValue,
) error {
	if required, stated := schema.Required.Get(); stated {
		for _, name := range required {
			if _, answered := content[name]; !answered {
				return fmt.Errorf("acp: the form requires %q and the answer does not carry it", name)
			}
		}
	}

	// Sorted, so that an answer wrong in two places names the same one every run.
	for _, name := range slices.Sorted(maps.Keys(content)) {
		declared, known := schema.Properties[name]
		if !known {
			continue
		}
		if err := validateElicitationValue(declared, content[name]); err != nil {
			return fmt.Errorf("acp: %q in the form answer: %w", name, err)
		}
	}
	return nil
}

func validateElicitationValue(declared ElicitationPropertySchema, value ElicitationContentValue) error {
	switch declared := declared.(type) {
	case *StringPropertySchema:
		return validateElicitationString(declared, value)
	case *IntegerPropertySchema:
		return validateElicitationInteger(declared, value)
	case *NumberPropertySchema:
		return validateElicitationNumber(declared, value)
	case *BooleanPropertySchema:
		if _, ok := value.(*ElicitationContentValueBoolean); !ok {
			return wrongElicitationType("boolean", value)
		}
		return nil
	case *MultiSelectPropertySchema:
		return validateElicitationMultiSelect(declared, value)
	default:
		return nil
	}
}

func validateElicitationString(declared *StringPropertySchema, value ElicitationContentValue) error {
	text, ok := value.(*ElicitationContentValueString)
	if !ok {
		return wrongElicitationType("string", value)
	}
	answer := string(*text)

	// Code points rather than bytes: JSON Schema counts characters, and a form
	// asking for at most eight is not asking for at most eight bytes.
	length := uint32(utf8.RuneCountInString(answer)) //nolint:gosec // a form's answer is bounded by the message size.
	if minimum, stated := declared.MinLength.Get(); stated && length < minimum {
		return fmt.Errorf("%q is %d characters and the form asks for at least %d", answer, length, minimum)
	}
	if maximum, stated := declared.MaxLength.Get(); stated && length > maximum {
		return fmt.Errorf("%q is %d characters and the form allows at most %d", answer, length, maximum)
	}
	if choices, stated := declared.Enum.Get(); stated && !slices.Contains(choices, answer) {
		return fmt.Errorf("%q is not one of the choices the form offered: %v", answer, choices)
	}
	if titled, stated := declared.OneOf.Get(); stated {
		if !slices.ContainsFunc(titled, func(option EnumOption) bool { return option.Const == answer }) {
			return fmt.Errorf("%q is not one of the titled choices the form offered", answer)
		}
	}
	return nil
}

func validateElicitationInteger(declared *IntegerPropertySchema, value ElicitationContentValue) error {
	// A number that is not whole decodes as the number arm, so reaching here with
	// one is the answer disagreeing with the form rather than an arm being unlucky.
	number, ok := value.(*ElicitationContentValueInteger)
	if !ok {
		return wrongElicitationType("integer", value)
	}
	answer := int64(*number)
	if minimum, stated := declared.Minimum.Get(); stated && answer < minimum {
		return fmt.Errorf("%d is below the form's minimum of %d", answer, minimum)
	}
	if maximum, stated := declared.Maximum.Get(); stated && answer > maximum {
		return fmt.Errorf("%d is above the form's maximum of %d", answer, maximum)
	}
	return nil
}

func validateElicitationNumber(declared *NumberPropertySchema, value ElicitationContentValue) error {
	// JSON has one number type, so a whole number is a valid answer to a form
	// asking for a number — and it decodes as the integer arm, which tries first.
	var answer float64
	switch value := value.(type) {
	case *ElicitationContentValueNumber:
		answer = float64(*value)
	case *ElicitationContentValueInteger:
		answer = float64(*value)
	default:
		return wrongElicitationType("number", value)
	}
	if minimum, stated := declared.Minimum.Get(); stated && answer < minimum {
		return fmt.Errorf("%v is below the form's minimum of %v", answer, minimum)
	}
	if maximum, stated := declared.Maximum.Get(); stated && answer > maximum {
		return fmt.Errorf("%v is above the form's maximum of %v", answer, maximum)
	}
	return nil
}

func validateElicitationMultiSelect(
	declared *MultiSelectPropertySchema,
	value ElicitationContentValue,
) error {
	list, ok := value.(*ElicitationContentValueStringArray)
	if !ok {
		return wrongElicitationType("array of strings", value)
	}
	chosen := []string(*list)

	count := uint64(len(chosen))
	if minimum, stated := declared.MinItems.Get(); stated && count < minimum {
		return fmt.Errorf("%d chosen and the form asks for at least %d", count, minimum)
	}
	if maximum, stated := declared.MaxItems.Get(); stated && count > maximum {
		return fmt.Errorf("%d chosen and the form allows at most %d", count, maximum)
	}

	// The catch-all arm is skipped for the reason it exists: a set of items this
	// package cannot name has no membership it can check.
	switch items := declared.Items.(type) {
	case *StringMultiSelectItems:
		for _, choice := range chosen {
			if !slices.Contains(items.Enum, choice) {
				return fmt.Errorf("%q is not one of the choices the form offered: %v", choice, items.Enum)
			}
		}
	case *TitledMultiSelectItems:
		for _, choice := range chosen {
			if !slices.ContainsFunc(items.AnyOf, func(o EnumOption) bool { return o.Const == choice }) {
				return fmt.Errorf("%q is not one of the titled choices the form offered", choice)
			}
		}
	}
	return nil
}

func wrongElicitationType(wanted string, got ElicitationContentValue) error {
	return fmt.Errorf("the form asks for %s and the answer is %T", wanted, got)
}

// acceptedFormContent returns the content of an accepted answer, and whether
// there is any to check. A declined or cancelled elicitation carries no answer,
// and an accepted one need not: a form may ask for nothing.
func acceptedFormContent(
	response *CreateElicitationResponse,
) (map[string]ElicitationContentValue, bool) {
	if response == nil {
		return nil, false
	}
	accepted, ok := response.Value.(*ElicitationAcceptAction)
	if !ok {
		return nil, false
	}
	return accepted.Content.Get()
}
