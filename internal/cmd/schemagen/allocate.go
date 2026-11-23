package main

import (
	"fmt"
)

// allocate assigns Go names over the whole schema — every definition and every
// union arm, whether the manifest reaches it or not — so that growing the
// manifest cannot rename a type that was already published.
func (p *planner) allocate() error {
	for _, name := range sortedKeys(p.doc.Defs) {
		if p.bindings[name] != nil {
			continue
		}
		ident := goName(name)
		goTypeName := ident
		if p.internal[name] {
			goTypeName = unexport(ident)
		}
		if err := p.names.claim(goTypeName, definitionPointer+name); err != nil {
			return err
		}
		p.goNames[name] = goTypeName
		p.idents[name] = ident
	}

	// Count sole-payload uses before naming any arm: whether an arm can be its
	// payload's own type depends on how many other arms want the same payload.
	for _, name := range sortedKeys(p.doc.Defs) {
		for _, arm := range p.doc.Defs[name].Arms() {
			if payload := arm.armPayload(); payload != "" {
				p.armPayloads[payload]++
			}
		}
	}

	for _, name := range sortedKeys(p.doc.Defs) {
		if p.bindings[name] != nil {
			continue
		}
		if err := p.allocateArmsOf(name); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) allocateArmsOf(name string) error {
	schema := p.doc.Defs[name]
	shape, err := p.shape(name)
	if err != nil {
		// A definition no shape describes has no arms to name. Planning it, if the
		// manifest reaches it, reports why.
		return nil //nolint:nilerr // classification failures are planning's to report.
	}
	arms := schema.Arms()
	switch shape {
	case shapeFlattenedUnion:
		ident := p.idents[name] + flattenedSuffix
		if claimErr := p.names.claim(p.visible(name, ident), definitionPointer+name+" flattened union"); claimErr != nil {
			return claimErr
		}
		return p.allocateObjectArms(name, schema, arms)
	case shapeObjectUnion:
		return p.allocateObjectArms(name, schema, arms)
	case shapeValueUnion:
		for i, arm := range arms {
			ident := p.idents[name] + valueArmName(arm)
			goType := p.visible(name, ident)
			if claimErr := p.names.claim(goType, fmt.Sprintf("%s%s arm %d", definitionPointer, name, i)); claimErr != nil {
				return claimErr
			}
			p.arms[armKey{name, i}] = allocatedArm{goType: goType, ident: ident}
		}
		return nil
	default:
		return nil
	}
}

// visible spells ident as the definition's visibility requires.
func (p *planner) visible(definition, ident string) string {
	if p.internal[definition] {
		return unexport(ident)
	}
	return ident
}

func (p *planner) allocateObjectArms(name string, schema *Schema, arms []*Schema) error {
	disc := discriminant(schema, arms)
	for i, arm := range arms {
		allocated, err := p.allocateArm(name, disc, i, arm)
		if err != nil {
			return err
		}
		p.arms[armKey{name, i}] = allocated
	}
	return nil
}

// valueArmName names one arm of a value union after the arm's own title, or after
// the JSON type it accepts when the schema gives it no title.
func valueArmName(arm *Schema) string {
	if arm.Title != "" {
		return goName(arm.Title)
	}
	if base := arm.Type.Base(); base != "" {
		return goName(base)
	}
	return goName(jsonNull)
}

// allocateArm names one arm of an object union.
//
// The arm is its payload's own type when it has exactly one $ref payload, no
// properties of its own besides the discriminant, and no other arm anywhere in
// the schema shares that payload. Otherwise it needs a type of its own, named
// after its discriminant value or its title. A discriminant is an identifier the
// schema chose to tell this arm from its siblings, so it may stand alone; a title
// is prose that promises nothing about being unique, so a name taken from one is
// qualified by the union.
func (p *planner) allocateArm(union, disc string, index int, arm *Schema) (allocatedArm, error) {
	owner := fmt.Sprintf("%s%s arm %d", definitionPointer, union, index)
	payload := arm.armPayload()
	if !arm.isCatchAll() && payload != "" && p.armPayloads[payload] == 1 {
		return allocatedArm{goType: p.goNames[payload], ident: p.idents[payload]}, nil
	}

	tag, err := arm.armTag(disc)
	if err != nil {
		return allocatedArm{}, fmt.Errorf("%s: %w", owner, err)
	}
	if arm.Title == "" && tag == "" {
		return allocatedArm{}, fmt.Errorf("%s: cannot name an arm with neither a title nor a discriminant constant",
			owner)
	}
	fromTitle := arm.Title != ""
	preferred := goName(tag)
	if fromTitle {
		preferred = goName(arm.Title)
	}

	// An arm with no payload of its own is this union's cancelled case or its
	// catch-all rather than a thing, so it is named after the union too.
	if payload == "" || fromTitle {
		ident := p.idents[union] + preferred
		goType := p.visible(union, ident)
		if claimErr := p.names.claim(goType, owner); claimErr != nil {
			return allocatedArm{}, claimErr
		}
		return allocatedArm{goType: goType, ident: ident}, nil
	}
	goType, claimErr := p.names.claimArm(preferred, p.goNames[union], owner)
	if claimErr != nil {
		return allocatedArm{}, claimErr
	}
	ident := preferred
	if goType != preferred {
		ident = p.idents[union] + preferred
	}
	return allocatedArm{goType: goType, ident: ident}, nil
}
