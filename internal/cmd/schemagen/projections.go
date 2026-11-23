package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	if source.Kind != kindStruct {
		return fmt.Errorf("%s is not a struct, so there is nothing to project", spec.From)
	}
	if claimErr := p.names.claim(name, "projection of "+definitionPointer+spec.From); claimErr != nil {
		return claimErr
	}

	def := &Def{
		GoName:    name,
		Ident:     name,
		Kind:      kindStruct,
		CatchAll:  source.CatchAll,
		Flattened: source.Flattened,
		Doc: append([]string{
			name + " — the parameters of " + spec.From + " that a caller supplies.",
			"",
		}, wrap(76, spec.Why+" It is "+spec.From+" without "+
			joinQuoted(spec.Without)+", which the handle owns, so every other property survives — "+
			"including _meta.")...),
		Projection: &Projection{From: source},
	}

	for _, field := range source.Fields {
		if !slices.Contains(spec.Without, field.JSONName) {
			def.Fields = append(def.Fields, field)
		}
	}
	for _, property := range spec.Without {
		index := slices.IndexFunc(source.Fields, func(field *Field) bool { return field.JSONName == property })
		if index < 0 {
			return fmt.Errorf("%s has no %q property to leave out", spec.From, property)
		}
		def.Projection.Bound = append(def.Projection.Bound, source.Fields[index])
	}

	p.add(def)
	return nil
}

func joinQuoted(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	switch len(quoted) {
	case 0:
		return "nothing"
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}
