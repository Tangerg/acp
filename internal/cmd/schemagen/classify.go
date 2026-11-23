package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

// A defShape is what a schema definition is, decided once. Name allocation,
// planning and property typing all read this answer, so the three cannot
// disagree about a definition the way separate predicates with different
// precedence could.
type defShape int

const (
	shapeUnknown defShape = iota
	// shapeObjectUnion is a union whose arms are objects.
	shapeObjectUnion
	// shapeFlattenedUnion is an object with properties of its own and a union of
	// kind-specific shapes in the same JSON object.
	shapeFlattenedUnion
	shapeStringEnum
	shapeIntegerEnum
	shapeStruct
	shapeStringNewtype
	shapeNumericNewtype
	// shapeValueUnion is a union whose arms are different JSON shapes rather
	// than different values of one shape.
	shapeValueUnion
	// shapeRaw is a definition the schema constrains not at all.
	shapeRaw
)

// classify decides a definition's shape, refusing one no shape describes.
func classify(schema *Schema) (defShape, error) {
	arms := schema.Arms()
	switch {
	case len(arms) > 0 && len(schema.Properties) > 0:
		if schema.Type.Base() != jsonObject {
			return shapeUnknown, fmt.Errorf("properties and a union on a schema that is not an object: keywords %v",
				schema.Keywords)
		}
		if !armsAreObjects(arms) {
			return shapeUnknown, fmt.Errorf("an object flattens a union whose arms are not objects: keywords %v",
				schema.Keywords)
		}
		return shapeFlattenedUnion, nil
	case len(arms) > 0 && armsAreObjects(arms):
		return shapeObjectUnion, nil
	case len(arms) > 0 && allArmsHaveBase(arms, jsonString):
		return shapeStringEnum, nil
	case len(arms) > 0 && allArmsHaveBase(arms, jsonInteger):
		return shapeIntegerEnum, nil
	case len(arms) > 0:
		if base := schema.Type.Base(); base != "" {
			return shapeUnknown, fmt.Errorf("a %s whose arms are other JSON shapes: keywords %v", base, schema.Keywords)
		}
		return shapeValueUnion, nil
	case schema.Type.Base() == jsonObject || len(schema.Properties) > 0:
		return shapeStruct, nil
	case schema.Type.Base() == jsonString:
		return shapeStringNewtype, nil
	case schema.Type.Base() == jsonInteger || schema.Type.Base() == jsonNumber:
		return shapeNumericNewtype, nil
	case schema.isUnconstrained():
		return shapeRaw, nil
	default:
		return shapeUnknown, fmt.Errorf("unimplemented definition shape: keywords %v", schema.Keywords)
	}
}

// isUnion reports a shape that needs a generated selector: a Go interface cannot
// decode into itself. A flattened union is a struct, and its own codec reaches
// the union it carries.
func (s defShape) isUnion() bool {
	return s == shapeObjectUnion || s == shapeValueUnion
}

// armsAreObjects reports a union whose arms are objects, spelled as an object
// type or as a reference to one.
func armsAreObjects(arms []*Schema) bool {
	for _, arm := range arms {
		if arm.Type.Has(jsonObject) {
			return true
		}
		if arm.Type == nil && arm.armPayload() != "" {
			return true
		}
	}
	return false
}

// allArmsHaveBase reports a union every arm of which is the same JSON scalar
// type, which is what makes it an enumeration rather than a choice of shapes.
func allArmsHaveBase(arms []*Schema, base string) bool {
	for _, arm := range arms {
		if arm.Type.Base() != base {
			return false
		}
	}
	return true
}

// armPayload is the definition an arm carries, when it carries exactly one and
// declares nothing but constants beside it.
func (s *Schema) armPayload() string {
	if len(s.AllOf) != 1 {
		return ""
	}
	name := s.AllOf[0].RefName()
	if name == "" {
		return ""
	}
	for _, property := range s.Properties {
		if len(property.Const) == 0 {
			return "" // a property of the arm's own, beyond the discriminant
		}
	}
	return name
}

// armTag is the string constant an arm pins its union's discriminant to, or the
// empty string when the arm pins none.
func (s *Schema) armTag(discriminant string) (string, error) {
	property, declared := s.Properties[discriminant]
	if discriminant == "" || !declared || len(property.Const) == 0 {
		return "", nil
	}
	var tag string
	if err := json.Unmarshal(property.Const, &tag); err != nil {
		return "", fmt.Errorf("the %q constant %s is not a string", discriminant, property.Const)
	}
	return tag, nil
}

// isUntagged reports an arm that declares the discriminant as null, which is the
// arm a value without a discriminant belongs to.
func (s *Schema) isUntagged(discriminant string) bool {
	property, declared := s.Properties[discriminant]
	return declared && len(property.Const) == 0 && property.Type.Base() == "" && property.Type.Has(jsonNull)
}

// isCatchAll reports the open arm: the one whose `not` clause excludes the known
// arms' discriminant values. Its presence, not the choice of oneOf or anyOf, is
// what makes a union open.
func (s *Schema) isCatchAll() bool {
	return s.Not != nil
}

// reservedTags reads the discriminant values a catch-all arm's `not` clause
// excludes. The clause is the schema's statement of which values are reserved,
// so it is read rather than inferred from the sibling arms.
func (s *Schema) reservedTags(discriminant string) ([]string, error) {
	alternatives := s.Not.Arms()
	if len(alternatives) == 0 {
		alternatives = []*Schema{s.Not}
	}
	tags := make([]string, 0, len(alternatives))
	for i, alternative := range alternatives {
		tag, err := alternative.armTag(discriminant)
		if err != nil {
			return nil, fmt.Errorf("not alternative %d: %w", i, err)
		}
		if tag == "" {
			return nil, fmt.Errorf("not alternative %d does not pin %q to a constant", i, discriminant)
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// discriminant is the property that selects an arm: the declared discriminator,
// or the property every constant-carrying arm pins to a constant.
func discriminant(schema *Schema, arms []*Schema) string {
	if schema.Discriminator != nil {
		return schema.Discriminator.PropertyName
	}
	for _, arm := range arms {
		var names []string
		for _, name := range sortedKeys(arm.Properties) {
			if len(arm.Properties[name].Const) > 0 {
				names = append(names, name)
			}
		}
		if len(names) == 1 {
			return names[0]
		}
	}
	return ""
}

// isUnconstrained reports a schema that says nothing about the value at all. Only
// the keywords that constrain one count.
func (s *Schema) isUnconstrained() bool {
	return !slices.ContainsFunc(s.Keywords, func(keyword string) bool {
		return !annotationKeywords[keyword]
	})
}

// annotationKeywords leave every JSON value admissible.
var annotationKeywords = map[string]bool{
	"description":                    true,
	"title":                          true,
	"x-deserialize-default-on-error": true,
	"x-docs-ignore":                  true,
	"x-method":                       true,
	"x-side":                         true,
}
