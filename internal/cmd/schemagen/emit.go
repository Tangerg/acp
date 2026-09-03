package main

import (
	"bytes"
	"fmt"
	"go/format"
	"strconv"
	"strings"
)

// emitter writes the generated package.
//
// It answers no schema questions: every decision is already in the [Plan], and
// the whole of this file is the mapping from a resolved decision to Go source.
// That is why the planner is allowed to be strict — a construct it refuses never
// reaches here, so there is no shape this file has to guess at.
type emitter struct {
	buf  bytes.Buffer
	plan *Plan
}

func emit(plan *Plan) ([]byte, error) {
	e := &emitter{plan: plan}
	for _, def := range plan.Defs {
		e.definition(def)
	}
	body := e.buf.Bytes()

	var out bytes.Buffer
	out.WriteString(comment("", []string{
		generatedHeader,
		"",
		"Source: schema/schema.json, pinned to upstream release " + plan.SchemaTag + ".",
		fmt.Sprintf(
			"Scope: the transitive $ref closure of schema/manifest.json, which is %d of the schema's definitions.",
			len(plan.Closure)),
		"",
		"The doc comments are the published specification's own prose. A change to",
		"that prose therefore arrives through a reviewed schema update rather than",
		"through hand edits to this file.",
	}))
	out.WriteString("\npackage acp\n\n")
	out.WriteString("import (\n")
	for _, path := range []string{"encoding/json", "errors", "fmt", "slices"} {
		if importUsed(body, path) {
			out.WriteString("\t")
			out.WriteString(strconv.Quote(path))
			out.WriteString("\n")
		}
	}
	out.WriteString("\n\t\"github.com/Tangerg/acp/internal/wire\"\n)\n\n")
	out.Write(body)

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("the generated source does not parse: %w", err)
	}
	return formatted, nil
}

// importUsed decides the import list from the emitted body rather than by
// tracking every emission site. The generated file is one package with a fixed,
// tiny import set, and a bookkeeping flag per import is a thing that can be
// wrong in a way this cannot.
func importUsed(body []byte, path string) bool {
	name := path[strings.LastIndexByte(path, '/')+1:]
	return bytes.Contains(body, []byte(name+"."))
}

func (e *emitter) printf(format string, args ...any) {
	fmt.Fprintf(&e.buf, format, args...)
}

func (e *emitter) definition(def *Def) {
	switch def.Kind {
	case kindStruct:
		e.structDef(def)
	case kindNewtype:
		e.printf("%stype %s %s\n\n", comment("", def.Doc), def.GoName, def.GoBase)
	case kindNullArm:
		e.nullArm(def)
	case kindRawValue:
		e.rawValue(def)
	case kindStringUnion:
		e.stringUnion(def)
	case kindNumberUnion:
		e.numberUnion(def)
	case kindObjectUnion:
		e.objectUnion(def)
	case kindValueUnion:
		e.valueUnion(def)
	}
}

// -- string unions -----------------------------------------------------------

func (e *emitter) stringUnion(def *Def) {
	doc := def.Doc
	if def.Closed {
		doc = append(doc, "",
			"The schema lists these values and no others, so a value outside them is a",
			"decode error rather than a value to keep: a caller's switch over this type",
			"was promised to be exhaustive.")
	} else {
		doc = append(doc, "",
			"The schema's last arm is a bare string, so this union is open: a value",
			"outside the constants below is valid and is kept as received.")
	}
	e.printf("%stype %s string\n\n", comment("", doc), def.GoName)

	if len(def.Values) > 0 {
		e.printf("const (\n")
		for i, value := range def.Values {
			if i > 0 {
				e.printf("\n")
			}
			e.printf("%s\t%s %s = %q\n", comment("\t", value.Doc), value.GoName, def.GoName, value.Value)
		}
		e.printf(")\n\n")
	}
	if !def.Closed {
		return
	}

	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	e.printf("\tif err := x.validate(); err != nil {\n\t\treturn nil, err\n\t}\n")
	e.printf("\treturn json.Marshal(string(x))\n}\n\n")

	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tvalue, err := wire.UnmarshalValue[string](data)\n")
	e.printf("\tif err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\tparsed := %s(value)\n", def.GoName)
	e.printf("\tif err := parsed.validate(); err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\t*x = parsed\n\treturn nil\n}\n\n")

	e.printf("func (x %s) validate() error {\n", def.GoName)
	e.printf("\tswitch x {\n\tcase ")
	names := make([]string, len(def.Values))
	for i, value := range def.Values {
		names[i] = value.GoName
	}
	e.printf("%s:\n\t\treturn nil\n", strings.Join(names, ", "))
	e.printf("\tdefault:\n\t\treturn fmt.Errorf(\"acp: %%q is not a %s the schema defines\", string(x))\n", def.GoName)
	e.printf("\t}\n}\n\n")
}

// -- unconstrained values ----------------------------------------------------

// A definition the schema constrains not at all is a JSON value of any shape, so
// it is the raw bytes under a name of its own.
//
// The codec is explicit because a named type does not inherit json.RawMessage's:
// without it the underlying byte slice would encode as base64, which is the wrong
// answer arrived at silently.
func (e *emitter) rawValue(def *Def) {
	e.printf("%stype %s json.RawMessage\n\n", comment("", def.Doc), def.GoName)

	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	e.printf("\tif len(x) == 0 {\n\t\treturn []byte(\"null\"), nil\n\t}\n")
	e.printf("\treturn x, nil\n}\n\n")

	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\t*x = append((*x)[:0], data...)\n\treturn nil\n}\n\n")
}

// -- value unions ------------------------------------------------------------

// A value union's arms are different JSON shapes, so there is no discriminant to
// read. They are tried in schema order and the first that decodes wins, which is
// what anyOf asks for — except that the null arm goes first, because every other
// arm's decoder refuses null.
func (e *emitter) valueUnion(def *Def) {
	arms := make([]string, len(def.Arms))
	for i, arm := range def.Arms {
		arms[i] = arm.GoType
	}
	doc := unionDoc(def.GoName, arms, def.Description)
	doc = append(doc, "",
		"The arms are different JSON shapes rather than different values of one, so",
		"there is no discriminant: a value is offered to each arm in schema order and",
		"belongs to the first that accepts it.")
	e.printf("%stype %s interface {\n\tis%s()\n}\n\n", comment("", doc), def.GoName, def.Ident)
	for _, arm := range def.Arms {
		e.printf("func (*%s) is%s() {}\n", arm.GoType, def.Ident)
	}
	e.printf("\n")

	e.printf("// marshal%s encodes whichever arm the value is.\n", def.Ident)
	e.printf("func marshal%s(value %s) ([]byte, error) {\n", def.Ident, def.GoName)
	e.printf("\tswitch value := value.(type) {\n")
	for _, arm := range def.Arms {
		e.printf("\tcase *%s:\n", arm.GoType)
		e.printf("\t\treturn json.Marshal(value)\n")
	}
	e.printf("\tcase nil:\n\t\treturn nil, errors.New(\"acp: %s is required and none was set\")\n", def.GoName)
	e.printf("\tdefault:\n\t\treturn nil, fmt.Errorf(\"acp: %%T is not an arm of %s\", value)\n", def.GoName)
	e.printf("\t}\n}\n\n")

	e.printf("// unmarshal%s selects the arm the value belongs to.\n", def.Ident)
	e.printf("func unmarshal%s(data json.RawMessage) (%s, error) {\n", def.Ident, def.GoName)
	for _, arm := range def.Arms {
		if !arm.IsNull {
			continue
		}
		e.printf("\tif wire.IsNull(data) {\n\t\treturn &%s{}, nil\n\t}\n\n", arm.GoType)
	}
	for _, arm := range def.Arms {
		if arm.IsNull {
			continue
		}
		e.printf("\tif value, err := wire.UnmarshalValue[%s](data); err == nil {\n", arm.GoType)
		e.printf("\t\treturn &value, nil\n\t}\n")
	}
	e.printf("\n\treturn nil, errors.New(\"acp: no %s arm matches this value\")\n}\n\n", def.GoName)
}

// The null arm carries nothing, so it is a type only so that it can implement the
// union's interface. It needs its own codec: the generic one refuses null, which
// is the whole of what this arm accepts.
func (e *emitter) nullArm(def *Def) {
	e.printf("%stype %s struct{}\n\n", comment("", def.Doc), def.GoName)
	e.printf("func (%s) MarshalJSON() ([]byte, error) {\n\treturn []byte(\"null\"), nil\n}\n\n", def.GoName)
	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tif !wire.IsNull(data) {\n")
	e.printf("\t\treturn errors.New(\"acp: this %s arm accepts only null\")\n\t}\n", def.GoName)
	e.printf("\t*x = %s{}\n\treturn nil\n}\n\n", def.GoName)
}

// -- numeric unions ----------------------------------------------------------

func (e *emitter) numberUnion(def *Def) {
	doc := def.Doc
	if def.Closed {
		doc = append(doc, "",
			"The schema lists these values and no others, so a value outside them is a",
			"decode error rather than a value to keep.")
	} else {
		doc = append(doc, "",
			"The schema's last arm carries no constant, so this union is open: a value",
			"outside the constants below is valid and is kept as received. The Go type is",
			"the arms' own format, because a wider one would admit values that cannot be",
			"encoded.")
	}
	e.printf("%stype %s %s\n\n", comment("", doc), def.GoName, def.GoBase)

	if len(def.Values) == 0 {
		return
	}
	e.printf("const (\n")
	for i, value := range def.Values {
		if i > 0 {
			e.printf("\n")
		}
		e.printf("%s\t%s %s = %s\n", comment("\t", value.Doc), value.GoName, def.GoName, value.Value)
	}
	e.printf(")\n\n")

	if !def.Closed {
		return
	}
	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	e.printf("\tif err := x.validate(); err != nil {\n\t\treturn nil, err\n\t}\n")
	e.printf("\treturn json.Marshal(%s(x))\n}\n\n", def.GoBase)

	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tvalue, err := wire.UnmarshalValue[%s](data)\n", def.GoBase)
	e.printf("\tif err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\tparsed := %s(value)\n", def.GoName)
	e.printf("\tif err := parsed.validate(); err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\t*x = parsed\n\treturn nil\n}\n\n")

	e.printf("func (x %s) validate() error {\n", def.GoName)
	e.printf("\tswitch x {\n\tcase ")
	names := make([]string, len(def.Values))
	for i, value := range def.Values {
		names[i] = value.GoName
	}
	e.printf("%s:\n\t\treturn nil\n", strings.Join(names, ", "))
	e.printf("\tdefault:\n\t\treturn fmt.Errorf(\"acp: %%d is not a %s the schema defines\", %s(x))\n",
		def.GoName, def.GoBase)
	e.printf("\t}\n}\n\n")
}

// -- object unions -----------------------------------------------------------

func (e *emitter) objectUnion(def *Def) {
	arms := make([]string, len(def.Arms))
	for i, arm := range def.Arms {
		arms[i] = arm.GoType
	}
	doc := unionDoc(def.GoName, arms, def.Description)
	if def.Open {
		doc = append(doc, "",
			"The union is open: the schema gives it a catch-all arm whose `not` clause",
			"reserves the known discriminant values, so a value carrying an unknown one",
			"decodes into that arm with its properties intact.")
	} else {
		doc = append(doc, "",
			"The union is closed: the schema defines these arms and no others, so a value",
			"matching none of them is a decode error. Openness is the schema's to grant,",
			"and it grants it elsewhere by defining a catch-all arm.")
	}
	e.printf("%stype %s interface {\n\tis%s()\n}\n\n", comment("", doc), def.GoName, def.Ident)
	for _, arm := range def.Arms {
		e.printf("func (*%s) is%s() {}\n", arm.GoType, def.Ident)
	}
	e.printf("\n")
	e.unionMarshal(def)
	e.unionUnmarshal(def)
}

func (e *emitter) unionMarshal(def *Def) {
	e.printf("// marshal%s writes the arm together with the discriminant the union owns.\n", def.Ident)
	e.printf("func marshal%s(value %s) ([]byte, error) {\n", def.Ident, def.GoName)
	e.printf("\tswitch value := value.(type) {\n")
	for _, arm := range def.Arms {
		e.printf("\tcase *%s:\n", arm.GoType)
		switch {
		case arm.Tag != "":
			e.printf("\t\treturn wire.TagObject(%q, %q, value)\n", def.Discriminant, arm.Tag)
		default:
			e.printf("\t\treturn json.Marshal(value)\n")
		}
	}
	e.printf("\tcase nil:\n\t\treturn nil, errors.New(\"acp: %s is required and none was set\")\n", def.GoName)
	e.printf("\tdefault:\n\t\treturn nil, fmt.Errorf(\"acp: %%T is not an arm of %s\", value)\n", def.GoName)
	e.printf("\t}\n}\n\n")
}

func (e *emitter) unionUnmarshal(def *Def) {
	var tagged, untagged []*Arm
	var catchAll *Arm
	for _, arm := range def.Arms {
		switch {
		case arm.CatchAll:
			catchAll = arm
		case arm.Tag != "":
			tagged = append(tagged, arm)
		default:
			untagged = append(untagged, arm)
		}
	}

	e.printf("// unmarshal%s selects the arm the value belongs to.\n", def.Ident)
	// The parameter is json.RawMessage rather than []byte so that this function
	// can be handed to wire.UnmarshalSliceFunc, whose element decoder that is:
	// Go's function types are identical only when their parameter types are.
	e.printf("func unmarshal%s(data json.RawMessage) (%s, error) {\n", def.Ident, def.GoName)
	// Every arm of every object union is an object, so a value that is not one
	// belongs to no arm. Decoding it up front reports that once, rather than
	// leaving the caller to read whichever arm happened to be tried last.
	if len(tagged) > 0 || catchAll != nil {
		e.printf("\tobject, objectErr := wire.DecodeObject(data)\n")
	} else {
		e.printf("\t_, objectErr := wire.DecodeObject(data)\n")
	}
	e.printf("\tif objectErr != nil {\n\t\treturn nil, objectErr\n\t}\n")

	if len(tagged) > 0 || catchAll != nil {
		e.printf("\n\tif tag, ok := wire.Tag(object, %q); ok {\n", def.Discriminant)
		switch len(tagged) {
		case 0:
		case 1:
			// A one-case switch is a conditional spelled the long way.
			e.printf("\t\tif tag == %q {\n", tagged[0].Tag)
			e.printf("\t\t\tvalue, err := wire.UnmarshalValue[%s](data)\n", tagged[0].GoType)
			e.printf("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			e.printf("\t\t\treturn &value, nil\n")
			e.printf("\t\t}\n")
		default:
			e.printf("\t\tswitch tag {\n")
			for _, arm := range tagged {
				e.printf("\t\tcase %q:\n", arm.Tag)
				e.printf("\t\t\tvalue, err := wire.UnmarshalValue[%s](data)\n", arm.GoType)
				e.printf("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
				e.printf("\t\t\treturn &value, nil\n")
			}
			e.printf("\t\t}\n")
		}
		switch {
		case catchAll != nil:
			e.printf("\n%s", comment("\t\t", []string{
				"A discriminant no known arm claims. The catch-all arm's `not` clause is",
				"exactly this case, and a known arm's discriminant with a payload that does",
				"not match it has already failed above rather than landing here.",
			}))
			e.printf("\t\tvalue, err := wire.UnmarshalValue[%s](data)\n", catchAll.GoType)
			e.printf("\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
			e.printf("\t\treturn &value, nil\n")
		case len(untagged) == 0:
			e.printf("\n\t\treturn nil, fmt.Errorf(\"acp: %%q is not a %s the schema defines\", tag)\n", def.GoName)
		default:
			e.printf("\n%s", comment("\t\t", []string{
				"A discriminant no known arm claims. The remaining arms declare none, so",
				"they are tried below in schema order, which is what anyOf asks for.",
			}))
		}
		e.printf("\t}\n")
	}

	for _, arm := range untagged {
		e.printf("\n\tif value, err := wire.UnmarshalValue[%s](data); err == nil {\n", arm.GoType)
		e.printf("\t\treturn &value, nil\n\t}\n")
	}

	if len(untagged) == 0 && (len(tagged) > 0 || catchAll != nil) {
		e.printf("\n\treturn nil, wire.At(%q, wire.ErrRequired)\n", def.Discriminant)
	} else {
		e.printf("\n\treturn nil, errors.New(\"acp: no %s arm matches this value\")\n", def.GoName)
	}
	e.printf("}\n\n")
}

// -- structs -----------------------------------------------------------------

func (e *emitter) structDef(def *Def) {
	if len(def.Fields) == 0 && def.Embeds == "" && def.Flattened == "" && !def.Retained {
		// A type the schema gives no properties. It is still an object on the wire,
		// and gofumpt spells an empty struct without a body.
		e.printf("%stype %s struct{}\n\n", comment("", def.Doc), def.GoName)
		e.structUnmarshal(def)
		e.structMarshal(def)
		return
	}
	e.printf("%stype %s struct {\n", comment("", def.Doc), def.GoName)
	if def.Embeds != "" {
		e.printf("\t%s\n", def.Embeds)
	}
	for i, field := range def.Fields {
		if i > 0 || def.Embeds != "" {
			e.printf("\n")
		}
		e.printf("%s\t%s %s `json:%q`\n", comment("\t", field.Doc), field.GoName, field.GoType, jsonTag(field))
	}
	if def.Flattened != "" {
		if len(def.Fields) > 0 {
			e.printf("\n")
		}
		e.printf("%s", comment("\t", []string{
			flattenedSuffix + " is the kind-specific part of this type, which the schema puts in the",
			"same JSON object as the properties above rather than under a property of its",
			"own. A Go struct cannot be several shapes at once, so it holds the choice",
			"here and the encoder flattens it back.",
		}))
		e.printf("\t%s %s `json:\"-\"`\n", flattenedSuffix, def.Flattened)
	}
	if def.Retained {
		e.printf("\n%s", comment("\t", []string{
			"Extra holds every property this arm's schema does not declare, exactly as",
			"received. They are the sender's payload: the schema gives the arm",
			"additionalProperties, so anything it does not name belongs to whoever sent",
			"it and has to survive a decode and re-encode.",
		}))
		e.printf("\tExtra map[string]json.RawMessage `json:\"-\"`\n")
	}
	e.printf("}\n\n")

	if def.Embeds != "" {
		// Explicit delegation rather than the promotion an embedded field would
		// give: a promoted MarshalJSON is easy to read as the wrapper's own and
		// stops being right the moment the wrapper gains a property.
		e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
		e.printf("\treturn x.%s.UnmarshalJSON(data)\n}\n\n", def.Embeds)
		e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
		e.printf("\treturn x.%s.MarshalJSON()\n}\n\n", def.Embeds)
		return
	}

	e.structUnmarshal(def)
	e.structMarshal(def)
	if def.Retained {
		e.retainedValidate(def)
	}
}

func jsonTag(field *Field) string {
	tag := field.JSONName
	// omitzero rather than omitempty: an absent Opt and an empty slice both have
	// to be distinguishable from a present zero value, and omitempty cannot do
	// either — it never omits a struct, and it always omits an empty array.
	if field.Opt || (field.Value.Kind == vSlice && !field.Required) {
		tag += ",omitzero"
	}
	return tag
}

func (e *emitter) structUnmarshal(def *Def) {
	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	if len(def.Fields) > 0 || def.Retained {
		e.printf("\tobject, objectErr := wire.DecodeObject(data)\n")
	} else {
		// A type with no properties is still an object, and a value that is not
		// one does not decode into it.
		e.printf("\t_, objectErr := wire.DecodeObject(data)\n")
	}
	e.printf("\tif objectErr != nil {\n\t\treturn objectErr\n\t}\n")
	e.printf("\t*x = %s{}\n", def.GoName)
	for _, field := range def.Fields {
		e.fieldUnmarshal(field)
	}
	if def.Flattened != "" {
		e.printf("\n\t{\n")
		e.printf("\t\tvalue, err := unmarshal%s(data)\n", def.FlattenedIdent)
		e.printf("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
		e.printf("\t\tx.%s = value\n\t}\n", flattenedSuffix)
	}
	if def.Retained {
		declared := make([]string, len(def.Fields))
		for i, field := range def.Fields {
			declared[i] = strconv.Quote(field.JSONName)
		}
		e.printf("\n\tx.Extra = wire.Extra(object, %s)\n", strings.Join(declared, ", "))
		e.printf("\n\treturn x.validate()\n}\n\n")
		return
	}
	e.printf("\n\treturn nil\n}\n\n")
}

func (e *emitter) fieldUnmarshal(field *Field) {
	e.printf("\n\tif raw, ok := object[%q]; ok {\n", field.JSONName)
	body := "\t\t"
	if field.Opt && field.Nullable {
		e.printf("\t\tif wire.IsNull(raw) {\n")
		e.printf("\t\t\tx.%s = OptNull[%s]()\n", field.GoName, field.Value.Go)
		e.printf("\t\t} else {\n")
		body = "\t\t\t"
	}

	e.printf("%svalue, err := %s\n", body, e.decodeExpr(field))
	e.printf("%sif err == nil {\n", body)
	if field.Opt {
		e.printf("%s\tx.%s = OptValue(value)\n", body, field.GoName)
	} else {
		e.printf("%s\tx.%s = value\n", body, field.GoName)
	}
	if recovery := e.recovery(field); recovery != "" {
		e.printf("%s} else {\n%s\t%s\n%s}\n", body, body, recovery, body)
	} else {
		e.printf("%s}\n", body)
	}

	if field.Opt && field.Nullable {
		e.printf("\t\t}\n")
	}
	if absent := e.absent(field); absent != "" {
		e.printf("\t} else {\n\t\t%s\n\t}\n", absent)
		return
	}
	e.printf("\t}\n")
}

// decodeExpr is the call that turns one property's raw bytes into its value.
func (e *emitter) decodeExpr(field *Field) string {
	value := field.Value
	if value.Kind == vSlice {
		elem := value.Elem
		switch {
		case elem.IsUnion && field.Skip:
			return fmt.Sprintf("wire.UnmarshalSliceFuncSkippingInvalid(raw, unmarshal%s)", elem.Ident)
		case elem.IsUnion:
			return fmt.Sprintf("wire.UnmarshalSliceFunc(raw, unmarshal%s)", elem.Ident)
		case field.Skip:
			return fmt.Sprintf("wire.UnmarshalSliceSkippingInvalid[%s](raw)", elem.Go)
		default:
			return fmt.Sprintf("wire.UnmarshalSlice[%s](raw)", elem.Go)
		}
	}
	if value.IsUnion {
		return fmt.Sprintf("unmarshal%s(raw)", value.Ident)
	}
	return fmt.Sprintf("wire.UnmarshalValue[%s](raw)", value.Go)
}

// recovery is what a malformed property becomes, or the empty string when the
// value is simply left alone because the fallback is the absent state.
func (e *emitter) recovery(field *Field) string {
	assign := func(literal string) string {
		if field.Opt {
			return fmt.Sprintf("x.%s = OptValue(%s)", field.GoName, literal)
		}
		return fmt.Sprintf("x.%s = %s", field.GoName, literal)
	}
	switch field.Fallback {
	case fbNone:
		return fmt.Sprintf("return wire.At(%q, err)", field.JSONName)
	case fbDefault:
		return assign(field.DefaultLit)
	case fbEmptySlice:
		return assign(field.Value.Go + "{}")
	case fbAbsent:
		return ""
	default:
		return ""
	}
}

// absent is what an omitted property becomes.
func (e *emitter) absent(field *Field) string {
	switch {
	case field.Required:
		return fmt.Sprintf("return wire.At(%q, wire.ErrRequired)", field.JSONName)
	case field.DefaultLit != "":
		if field.Opt {
			return fmt.Sprintf("x.%s = OptValue(%s)", field.GoName, field.DefaultLit)
		}
		return fmt.Sprintf("x.%s = %s", field.GoName, field.DefaultLit)
	default:
		return ""
	}
}

func (e *emitter) structMarshal(def *Def) {
	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	if def.Retained {
		// A pointer receiver on validate, called from a value receiver: x is a
		// local and so addressable. It keeps every method that is not part of the
		// json interfaces on the pointer, which is the receiver an arm of a union
		// is used through.
		e.printf("\tif err := x.validate(); err != nil {\n\t\treturn nil, err\n\t}\n\n")
	}
	e.printf("\tvar writer wire.ObjectWriter\n")
	for _, field := range def.Fields {
		e.fieldMarshal(field)
	}
	if def.Flattened != "" {
		e.printf("\t{\n")
		e.printf("\t\traw, err := marshal%s(x.%s)\n", def.FlattenedIdent, flattenedSuffix)
		e.printf("\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
		e.printf("\t\twriter.Embed(raw)\n\t}\n")
	}
	if def.Retained {
		e.printf("\twriter.SetExtra(x.Extra)\n")
	}
	e.printf("\n\treturn writer.Bytes()\n}\n\n")
}

func (e *emitter) fieldMarshal(field *Field) {
	encode := e.encodeExpr(field)
	if encode == "" {
		// Nothing about the value needs the union or slice treatment, so the
		// writer's own encoding is the whole of it.
		if field.Opt {
			e.printf("\twriter.SetOptional(%q, x.%s)\n", field.JSONName, field.GoName)
		} else {
			e.printf("\twriter.Set(%q, x.%s)\n", field.JSONName, field.GoName)
		}
		return
	}

	switch {
	case field.Opt:
		e.printf("\tif value, ok := x.%s.Get(); ok {\n", field.GoName)
		e.printf("\t\traw, err := %s\n", strings.ReplaceAll(encode, "$", "value"))
		e.printf("\t\tif err != nil {\n\t\t\treturn nil, wire.At(%q, err)\n\t\t}\n", field.JSONName)
		e.printf("\t\twriter.SetRaw(%q, raw)\n", field.JSONName)
		e.printf("\t} else if x.%s.IsNull() {\n", field.GoName)
		e.printf("\t\twriter.SetRaw(%q, []byte(\"null\"))\n", field.JSONName)
		e.printf("\t}\n")
	case field.Value.Kind == vSlice && !field.Required:
		// A nil optional slice is the absent state and is left out entirely; an
		// empty one is present and encodes as [].
		e.printf("\tif x.%s != nil {\n", field.GoName)
		e.printf("\t\traw, err := %s\n", strings.ReplaceAll(encode, "$", "x."+field.GoName))
		e.printf("\t\tif err != nil {\n\t\t\treturn nil, wire.At(%q, err)\n\t\t}\n", field.JSONName)
		e.printf("\t\twriter.SetRaw(%q, raw)\n", field.JSONName)
		e.printf("\t}\n")
	default:
		e.printf("\t{\n")
		e.printf("\t\traw, err := %s\n", strings.ReplaceAll(encode, "$", "x."+field.GoName))
		e.printf("\t\tif err != nil {\n\t\t\treturn nil, wire.At(%q, err)\n\t\t}\n", field.JSONName)
		e.printf("\t\twriter.SetRaw(%q, raw)\n", field.JSONName)
		e.printf("\t}\n")
	}
}

// encodeExpr returns the call that encodes one property, with $ standing for the
// value, or the empty string when the writer can encode the value itself.
//
// It cannot always: a union's discriminant belongs to the union rather than to
// the arm, and a required array has to encode as [] where a nil Go slice would
// encode as null.
func (e *emitter) encodeExpr(field *Field) string {
	value := field.Value
	if value.Kind == vSlice {
		if value.Elem.IsUnion {
			return fmt.Sprintf("wire.MarshalSliceFunc($, marshal%s)", value.Elem.Ident)
		}
		return "wire.MarshalSlice($)"
	}
	if value.IsUnion {
		return fmt.Sprintf("marshal%s($)", value.Ident)
	}
	return ""
}

func (e *emitter) retainedValidate(def *Def) {
	e.printf("%s", comment("", []string{
		"validate enforces the `not` clause of the catch-all arm: the discriminant",
		"values the known arms claim are reserved, and a value carrying one is a",
		"malformed known arm rather than a custom one. Both the decoder and the",
		"encoder check it, so the rule holds in each direction.",
	}))
	e.printf("func (x *%s) validate() error {\n", def.GoName)
	reserved := make([]string, len(def.ReservedTags))
	for i, tag := range def.ReservedTags {
		reserved[i] = strconv.Quote(tag)
	}
	e.printf("\tif slices.Contains([]string{%s}, x.%s) {\n", strings.Join(reserved, ", "), goName(def.TagProperty))
	e.printf("\t\treturn wire.At(%q, fmt.Errorf(\n", def.TagProperty)
	e.printf("\t\t\t\"acp: %%q is reserved by a known arm, but the value does not match that arm\", x.%s))\n",
		goName(def.TagProperty))
	e.printf("\t}\n\treturn nil\n}\n\n")
}
