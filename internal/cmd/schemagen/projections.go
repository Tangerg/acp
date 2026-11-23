package main

import (
	"fmt"
	"slices"
	"strconv"
)

// planProjection derives a handle-facing params type from a wire request type,
// and the conversion back.
//
// Both are generated, because the projection is the wire type minus the
// identifiers the handle owns: every other property survives, and a schema bump
// that adds one reaches the params type and the conversion in the same commit.
func (p *planner) planProjection(name string, spec *ProjectionSpec) error {
	source, err := p.plan(spec.From)
	if err != nil {
		return err
	}
	sourceBody, isStruct := source.Body.(*structBody)
	if !isStruct {
		return fmt.Errorf("%s is not a struct, so there is nothing to project", spec.From)
	}
	if claimErr := p.names.claim(name, "projection of "+definitionPointer+spec.From); claimErr != nil {
		return claimErr
	}

	body := &structBody{
		CatchAll:   sourceBody.CatchAll,
		Flattened:  sourceBody.Flattened,
		Projection: &Projection{From: source},
	}
	def := &Def{
		GoName: name,
		Ident:  name,
		Body:   body,
		Doc: append([]string{
			name + " — the parameters of " + spec.From + " that a caller supplies.",
			"",
		}, wrap(spec.Why+" It is "+spec.From+" without "+
			quotedList(spec.Without)+", which the handle owns, so every other property survives — "+
			"including _meta.")...),
	}

	for _, field := range sourceBody.Fields {
		if !slices.Contains(spec.Without, field.JSONName) {
			body.Fields = append(body.Fields, field)
		}
	}
	for _, property := range spec.Without {
		index := slices.IndexFunc(sourceBody.Fields, func(field *Field) bool { return field.JSONName == property })
		if index < 0 {
			return fmt.Errorf("%s has no %q property to leave out", spec.From, property)
		}
		body.Projection.Bound = append(body.Projection.Bound, sourceBody.Fields[index])
	}

	p.add(def)
	return nil
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return englishList(quoted, "and")
}
