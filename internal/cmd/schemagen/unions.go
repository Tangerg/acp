package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// retainedField names the field a catch-all arm keeps a sender's undeclared
// properties in.
const retainedField = "Extra"

func (p *planner) planStringUnion(def *Def, arms []*Schema) error {
	body := new(stringEnumBody)
	def.Body = body
	for i, arm := range arms {
		if len(arm.Const) == 0 {
			// The bare-typed arm. Its presence is how the schema says the union is
			// open, so a value outside the list is valid and is kept as received.
			body.Open = true
			continue
		}
		var value string
		if err := json.Unmarshal(arm.Const, &value); err != nil {
			return fmt.Errorf("arm %d: a constant that is not a string: %s", i, arm.Const)
		}
		if err := p.addEnumValue(def, &body.enumeration, goName(value), value, arm.Description); err != nil {
			return err
		}
	}
	return nil
}

// planNumberUnion is planStringUnion for an integer-valued enumeration. Its
// constants are named after each arm's title, because an integer reads poorly
// as a name: ErrorCode's arms are named after "Parse error", not after -32700.
func (p *planner) planNumberUnion(def *Def, arms []*Schema) error {
	body := new(numberEnumBody)
	def.Body = body
	for i, arm := range arms {
		goType, err := goNumericType(arm)
		if err != nil {
			return err
		}
		if body.Base == nil {
			body.Base = &ValueType{Kind: vScalar, Go: goType}
		}
		if body.Base.Go != goType {
			return fmt.Errorf("arms disagree about the numeric format: %s and %s", body.Base.Go, goType)
		}
		if len(arm.Const) == 0 {
			body.Open = true
			continue
		}
		var value json.Number
		if decodeErr := json.Unmarshal(arm.Const, &value); decodeErr != nil {
			return fmt.Errorf("arm %d: a constant that is not a number: %s", i, arm.Const)
		}
		if arm.Title == "" {
			return fmt.Errorf("arm %d: a constant arm with the value %s and no title to name it after", i, value)
		}
		if addErr := p.addEnumValue(def, &body.enumeration, goName(arm.Title), value.String(),
			arm.Description); addErr != nil {
			return addErr
		}
	}
	return nil
}

func (p *planner) addEnumValue(def *Def, values *enumeration, suffix, value, description string) error {
	name := def.GoName + suffix
	if err := p.names.claim(name, fmt.Sprintf("%s%s value %s", definitionPointer, def.SchemaName, value)); err != nil {
		return err
	}
	values.Values = append(values.Values, &EnumValue{GoName: name, Value: value, Doc: docComment(name, description)})
	return nil
}

// unionOwnedProperty is the discriminant a flattened union owns when the object
// also declares it as a property of its own, or the empty string.
//
// The declaration says what the property is when no arm's constant matches. Only
// the shape whose meaning the union already implements is accepted: an optional,
// nullable string that recovers from a malformed value to absent, which is
// exactly when the union's tag is absent.
func unionOwnedProperty(schema *Schema) (string, error) {
	disc := discriminant(schema, schema.Arms())
	declared, ok := schema.Properties[disc]
	if disc == "" || !ok {
		return "", nil
	}
	switch {
	case slices.Contains(schema.Required, disc):
		return "", fmt.Errorf("the discriminant %q is also a required property, which is not implemented", disc)
	case declared.Type.Base() != jsonString || !declared.Nullable() || !declared.DefaultOnError:
		return "", fmt.Errorf("the discriminant %q is also a property that is not an optional, recoverable "+
			"string, which is not implemented", disc)
	default:
		return disc, nil
	}
}

// planFlattenedUnion plans the union a struct carries in its own JSON object. It
// is a type of its own, held in a field, because a Go struct cannot be several
// shapes at once; the schema gives the construct no name, so it is named after
// the struct.
func (p *planner) planFlattenedUnion(def *Def, body *structBody, schema *Schema) error {
	for _, field := range body.Fields {
		if field.GoName == flattenedSuffix {
			return fmt.Errorf("the type already declares a %s property, so its flattened union has no name",
				field.JSONName)
		}
	}
	ident := def.Ident + flattenedSuffix
	union := &Def{
		SchemaName:  def.SchemaName,
		GoName:      p.visible(def.SchemaName, ident),
		Ident:       ident,
		Description: schema.Description,
	}
	if err := p.planObjectUnion(union, schema); err != nil {
		return err
	}
	p.add(union)
	body.Flattened = union
	return nil
}

// planValueUnion plans a union whose arms are different JSON shapes. Each arm is
// a Go type of its own, because only a named type can implement the union's
// interface, and it is named after the union as well as itself: a value union's
// titles are shape words that would be poor names on their own.
func (p *planner) planValueUnion(def *Def, arms []*Schema) error {
	body := new(valueUnionBody)
	def.Body = body
	for i, arm := range arms {
		allocated := p.arms[armKey{def.SchemaName, i}]
		armDef := &Def{
			GoName: allocated.goType,
			Ident:  allocated.ident,
			Doc:    armDoc(allocated.goType, "one arm of "+def.GoName, arm.Description),
		}
		if arm.Type.Base() == "" && arm.Type.Has(jsonNull) {
			armDef.Body = nullArmBody{}
			p.add(armDef)
			body.Arms = append(body.Arms, &Arm{GoType: allocated.goType, IsNull: true})
			continue
		}
		base, err := p.valueType(arm)
		if err != nil {
			return fmt.Errorf("arm %d: %w", i, err)
		}
		armDef.Body = &newtypeBody{Base: base}
		p.add(armDef)
		body.Arms = append(body.Arms, &Arm{GoType: allocated.goType})
	}
	return nil
}

func armDoc(goType, role, description string) []string {
	doc := []string{goType + " — " + role + "."}
	if described := docComment("", description); len(described) > 0 {
		doc = append(doc, "")
		doc = append(doc, described...)
	}
	return doc
}

func (p *planner) planObjectUnion(def *Def, schema *Schema) error {
	arms := schema.Arms()
	// A union without a discriminant is an ordinary anyOf: its arms are tried in
	// schema order and the first that decodes wins.
	body := &objectUnionBody{Discriminant: discriminant(schema, arms)}
	def.Body = body

	common := slices.DeleteFunc(slices.Clone(schema.PropertyOrder), func(name string) bool {
		return name == body.Discriminant
	})
	var siblingTags []string
	for i, arm := range arms {
		tag, err := arm.armTag(body.Discriminant)
		if err != nil {
			return fmt.Errorf("arm %d: %w", i, err)
		}
		if tag != "" {
			siblingTags = append(siblingTags, tag)
		}
	}

	for i, arm := range arms {
		allocated := p.arms[armKey{def.SchemaName, i}]
		tag, _ := arm.armTag(body.Discriminant)
		planned := &Arm{
			Tag: tag, GoType: allocated.goType, CatchAll: arm.isCatchAll(), Untagged: arm.isUntagged(body.Discriminant),
		}
		if tag != "" {
			planned.TagConst = unexport(allocated.ident) + "Tag"
			if err := p.names.claim(planned.TagConst, fmt.Sprintf("%s%s arm %d tag", definitionPointer,
				def.SchemaName, i)); err != nil {
				return err
			}
		}
		body.Arms = append(body.Arms, planned)
		if planned.CatchAll {
			body.Open = true
		}

		payload := arm.armPayload()
		var err error
		switch {
		case planned.CatchAll:
			err = p.planCatchAllArm(allocated, arm, def, body.Discriminant, siblingTags, common)
		case payload != "" && allocated.goType == p.goNames[payload]:
			var target *Def
			target, err = p.plan(payload)
			if err == nil {
				err = checkArmPayload(target, body.Discriminant, common)
			}
		default:
			err = p.planWrapperArm(allocated, arm, def, body.Discriminant, payload, common)
		}
		if err != nil {
			return fmt.Errorf("arm %d: %w", i, err)
		}
	}
	return nil
}

// planWrapperArm plans an arm that needs a type of its own: one whose payload
// another arm also uses, so only the discriminant tells them apart, or one with
// no payload at all.
func (p *planner) planWrapperArm(
	allocated allocatedArm,
	arm *Schema,
	union *Def,
	discriminant, payload string,
	common []string,
) error {
	body := new(structBody)
	def := &Def{GoName: allocated.goType, Ident: allocated.ident, Body: body}
	doc := armDoc(allocated.goType, "one arm of "+union.GoName, arm.Description)

	for _, jsonName := range arm.PropertyOrder {
		if jsonName == discriminant {
			continue // the union writes and reads its own discriminant
		}
		if slices.Contains(common, jsonName) {
			return fmt.Errorf("arm %s redeclares common property %q", allocated.goType, jsonName)
		}
		field, err := p.planField(jsonName, arm.Properties[jsonName], slices.Contains(arm.Required, jsonName))
		if err != nil {
			return fmt.Errorf("property %q: %w", jsonName, err)
		}
		body.Fields = append(body.Fields, field)
	}

	if payload != "" {
		target, err := p.plan(payload)
		if err != nil {
			return fmt.Errorf("%s%s: %w", definitionPointer, payload, err)
		}
		if payloadErr := checkArmPayload(target, discriminant, common); payloadErr != nil {
			return payloadErr
		}
		if len(body.Fields) > 0 {
			return fmt.Errorf(
				"an arm with both a %s payload and properties of its own is not implemented", target.GoName)
		}
		body.Embeds = target.GoName
		doc = append(doc, "")
		doc = append(doc, wrap("It carries "+article(target.GoName, false)+" "+target.GoName+
			" and exists as a type of its own because more than one arm of "+union.GoName+
			" carries one: the discriminant is the only thing that tells them apart.")...)
	}

	def.Doc = doc
	p.add(def)
	return nil
}

// checkArmPayload refuses a payload whose keys collide with the union's: the
// discriminant is spliced in without re-parsing the payload, which is only safe
// when each part owns disjoint keys.
func checkArmPayload(payload *Def, discriminantName string, common []string) error {
	body, isStruct := payload.Body.(*structBody)
	if !isStruct {
		return nil
	}
	for _, field := range body.Fields {
		if field.JSONName == discriminantName {
			return fmt.Errorf("payload %s declares the discriminant property %q", payload.GoName, discriminantName)
		}
		if slices.Contains(common, field.JSONName) {
			return fmt.Errorf("payload %s redeclares common property %q", payload.GoName, field.JSONName)
		}
	}
	return nil
}

func (p *planner) planCatchAllArm(
	allocated allocatedArm,
	arm *Schema,
	union *Def,
	discriminant string,
	siblingTags, common []string,
) error {
	reserved, err := arm.reservedTags(discriminant)
	if err != nil {
		return err
	}
	if !sameSet(reserved, siblingTags) {
		return fmt.Errorf("the catch-all reserves %v but the known arms claim %v", reserved, siblingTags)
	}
	if arm.UnevaluatedProperties != nil && !arm.UnevaluatedProperties.IsTrue() {
		return errors.New("a catch-all arm that does not admit its sender's properties is not implemented")
	}

	doc := armDoc(allocated.goType, "the open catch-all arm of "+union.GoName, arm.Description)
	doc = append(doc, "")
	doc = append(doc, wrap(fmt.Sprintf(
		"The schema's `not` clause reserves the discriminant values %s's known arms claim, so a value carrying "+
			"one of those is a malformed known arm rather than a custom one and does not land here. Every "+
			"property neither this arm nor its containing object declares is kept in %s, because the schema "+
			"gives the arm additionalProperties: those keys are the sender's payload.", union.GoName, retainedField))...)

	catchAll := &CatchAll{TagProperty: discriminant, ReservedTags: reserved}
	body := &structBody{CatchAll: catchAll}
	def := &Def{GoName: allocated.goType, Ident: allocated.ident, Doc: doc, Body: body}
	for _, jsonName := range arm.PropertyOrder {
		if slices.Contains(common, jsonName) {
			return fmt.Errorf("catch-all %s redeclares common property %q", allocated.goType, jsonName)
		}
		field, fieldErr := p.planField(jsonName, arm.Properties[jsonName], slices.Contains(arm.Required, jsonName))
		if fieldErr != nil {
			return fmt.Errorf("property %q: %w", jsonName, fieldErr)
		}
		if field.GoName == retainedField {
			return fmt.Errorf("property %q collides with the field that retains the sender's properties", jsonName)
		}
		body.Fields = append(body.Fields, field)
		catchAll.ExcludedExtraKeys = append(catchAll.ExcludedExtraKeys, jsonName)
	}
	catchAll.ExcludedExtraKeys = append(catchAll.ExcludedExtraKeys, common...)
	if !slices.Contains(catchAll.ExcludedExtraKeys, discriminant) {
		return errors.New("a catch-all arm with no declared discriminant property")
	}

	alternatives, err := p.catchAllRequiredGroups(arm, body)
	if err != nil {
		return err
	}
	catchAll.Alternatives = alternatives
	p.add(def)
	return nil
}

func sameSet(left, right []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(left)), slices.Sorted(slices.Values(right)))
}

// catchAllRequiredGroups reads the alternatives a catch-all arm is itself a union
// of, as the property set each one requires.
//
// A catch-all arm has a schema of its own, and when that schema is a union a value
// has to satisfy one of its alternatives; otherwise this package would accept a
// message the published schema rejects. The alternatives are told apart by the
// properties they require, which must not be ones the arm declares, because the
// check reads the retained properties. A shape this cannot express stops
// generation instead of being dropped.
func (p *planner) catchAllRequiredGroups(arm *Schema, body *structBody) ([]catchAllAlternative, error) {
	alternatives := arm.Arms()
	if len(alternatives) == 0 {
		return nil, nil
	}

	declared := make(map[string]bool, len(body.Fields))
	for _, field := range body.Fields {
		declared[field.JSONName] = true
	}

	groups := make([]catchAllAlternative, 0, len(alternatives))
	for i, alternative := range alternatives {
		payload := alternative.armPayload()
		if payload == "" {
			return nil, fmt.Errorf(
				"alternative %d of a catch-all arm is not a single $ref, which is the only shape implemented", i)
		}
		referenced, defined := p.doc.Defs[payload]
		if !defined {
			return nil, fmt.Errorf("a catch-all arm's alternative refers to %s, which the schema does not define", payload)
		}
		if len(referenced.Required) == 0 {
			return nil, fmt.Errorf(
				"alternative %s of a catch-all arm requires no property, so nothing tells it from the others", payload)
		}
		for _, name := range referenced.Required {
			if declared[name] {
				return nil, fmt.Errorf(
					"alternative %s of a catch-all arm requires %q, which the arm declares itself; the check "+
						"reads the retained properties and cannot see a declared one", payload, name)
			}
		}
		if _, err := p.plan(payload); err != nil {
			return nil, fmt.Errorf("%s%s: %w", definitionPointer, payload, err)
		}
		groups = append(groups, catchAllAlternative{
			GoName:   p.goNames[payload],
			Required: slices.Clone(referenced.Required),
		})
	}
	return groups, nil
}
