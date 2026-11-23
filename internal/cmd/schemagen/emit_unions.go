package main

import (
	"fmt"
	"strconv"
	"strings"
)

// -- enumerations ------------------------------------------------------------

func (e *emitter) stringUnion(def *Def) {
	doc := def.Doc
	if def.Open {
		doc = append(doc, "",
			"The schema's last arm is a bare string, so this union is open: a value",
			"outside the constants below is valid and is kept as received.")
	} else {
		doc = append(doc, "",
			"The schema lists these values and no others, so a value outside them is a",
			"decode error rather than a value to keep.")
	}
	e.printf("%stype %s string\n\n", comment("", doc), def.GoName)
	e.enumConstants(def, "%q")
	if def.Open {
		e.namedValueUnmarshal(def, &ValueType{Kind: vScalar, Go: goString})
		return
	}
	e.closedEnumCodec(def, goString, "%q", "string(x)")
}

func (e *emitter) numberUnion(def *Def) {
	doc := def.Doc
	if def.Open {
		doc = append(doc, "",
			"The schema's last arm carries no constant, so this union is open: a value",
			"outside the constants below is valid and is kept as received. The Go type is",
			"the arms' own format, because a wider one would admit values that cannot be",
			"encoded.")
	} else {
		doc = append(doc, "",
			"The schema lists these values and no others, so a value outside them is a",
			"decode error rather than a value to keep.")
	}
	e.printf("%stype %s %s\n\n", comment("", doc), def.GoName, def.Base.Go)
	e.enumConstants(def, "%s")
	if def.Open {
		e.namedValueUnmarshal(def, def.Base)
		return
	}
	e.closedEnumCodec(def, def.Base.Go, "%d", def.Base.Go+"(x)")
}

func (e *emitter) enumConstants(def *Def, valueFormat string) {
	if len(def.Values) == 0 {
		return
	}
	e.printf("const (\n")
	for i, value := range def.Values {
		if i > 0 {
			e.printf("\n")
		}
		literal := value.Value
		if valueFormat == "%q" {
			literal = strconv.Quote(value.Value)
		}
		e.printf("%s\t%s %s = %s\n", comment("\t", value.Doc), value.GoName, def.GoName, literal)
	}
	e.printf(")\n\n")
}

// closedEnumCodec validates a closed enumeration in both directions. An
// enumeration with a single value has one encodable value, so its zero value
// encodes as that value rather than failing.
func (e *emitter) closedEnumCodec(def *Def, base, verb, underlying string) {
	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	if len(def.Values) == 1 {
		var zero string
		if base == goString {
			zero = `""`
		} else {
			zero = "0"
		}
		e.printf("\tif x == %s {\n\t\tx = %s\n\t}\n", zero, def.Values[0].GoName)
	}
	e.printf("\tif err := x.validate(); err != nil {\n\t\treturn nil, err\n\t}\n")
	e.printf("\treturn json.Marshal(%s)\n}\n\n", underlying)

	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tvalue, err := wire.UnmarshalValue[%s](data)\n", base)
	e.printf("\tif err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\tparsed := %s(value)\n", def.GoName)
	e.printf("\tif invalid := parsed.validate(); invalid != nil {\n\t\treturn invalid\n\t}\n")
	e.printf("\t*x = parsed\n\treturn nil\n}\n\n")

	names := make([]string, len(def.Values))
	for i, value := range def.Values {
		names[i] = value.GoName
	}
	e.printf("func (x %s) validate() error {\n", def.GoName)
	e.printf("\tswitch x {\n\tcase %s:\n\t\treturn nil\n", strings.Join(names, ", "))
	e.printf("\tdefault:\n\t\treturn fmt.Errorf(\"acp: %s is not a %s the schema defines\", %s)\n",
		verb, def.GoName, underlying)
	e.printf("\t}\n}\n\n")
}

// -- value unions ------------------------------------------------------------

// A value union's arms are different JSON shapes, so there is no discriminant to
// read. They are tried in schema order and the first that decodes wins, except
// that the null arm goes first, because every other arm's decoder refuses null.
func (e *emitter) valueUnion(def *Def) {
	doc := unionDoc(def.GoName, armTypes(def), def.Description)
	doc = append(doc, "",
		"The arms are different JSON shapes rather than different values of one, so",
		"there is no discriminant: a value is offered to each arm in schema order and",
		"belongs to the first that accepts it.")
	e.unionInterface(def, doc)

	e.printf("// marshal%s encodes whichever arm the value is.\n", def.Ident)
	e.printf("func marshal%s(value %s) ([]byte, error) {\n", def.Ident, def.GoName)
	e.printf("\tswitch value := value.(type) {\n")
	for _, arm := range def.Arms {
		e.printf("\tcase *%s:\n", arm.GoType)
		e.rejectNilUnionArm(def, arm)
		e.printf("\t\treturn json.Marshal(value)\n")
	}
	e.unionMarshalDefaults(def)

	e.printf("// unmarshal%s selects the arm the value belongs to.\n", def.Ident)
	e.printf("func unmarshal%s(data json.RawMessage) (%s, error) {\n", def.Ident, def.GoName)
	for _, arm := range def.Arms {
		if arm.IsNull {
			e.printf("\tif wire.IsNull(data) {\n\t\treturn &%s{}, nil\n\t}\n\n", arm.GoType)
		}
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
// union's interface. It needs its own codec: the generic one refuses null.
func (e *emitter) nullArm(def *Def) {
	e.printf("%stype %s struct{}\n\n", comment("", def.Doc), def.GoName)
	e.printf("func (%s) MarshalJSON() ([]byte, error) {\n\treturn []byte(\"null\"), nil\n}\n\n", def.GoName)
	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tif !wire.IsNull(data) {\n")
	e.printf("\t\treturn errors.New(\"acp: this %s arm accepts only null\")\n\t}\n", def.GoName)
	e.printf("\t*x = %s{}\n\treturn nil\n}\n\n", def.GoName)
}

// -- object unions -----------------------------------------------------------

func (e *emitter) objectUnion(def *Def) {
	doc := unionDoc(def.GoName, armTypes(def), def.Description)
	if def.Open {
		doc = append(doc, "",
			"The union is open: the schema gives it a catch-all arm whose `not` clause",
			"reserves the known discriminant values, so a value carrying an unknown one",
			"decodes into that arm with its properties intact.")
	} else {
		doc = append(doc, "",
			"The union is closed: the schema defines these arms and no others, so a value",
			"matching none of them is a decode error.")
	}
	e.unionInterface(def, doc)
	e.tagConstants(def)
	e.unionMarshal(def)
	e.unionUnmarshal(def)
}

func armTypes(def *Def) []string {
	arms := make([]string, len(def.Arms))
	for i, arm := range def.Arms {
		arms[i] = arm.GoType
	}
	return arms
}

func (e *emitter) unionInterface(def *Def, doc []string) {
	e.printf("%stype %s interface {\n\tis%s()\n}\n\n", comment("", doc), def.GoName, def.Ident)
	for _, arm := range def.Arms {
		e.printf("func (*%s) is%s() {}\n", arm.GoType, def.Ident)
	}
	e.printf("\n")
}

// tagConstants names each discriminant value once, for the codec and for the
// hand-written code that refers to an arm by its wire spelling.
func (e *emitter) tagConstants(def *Def) {
	var tagged []*Arm
	for _, arm := range def.Arms {
		if arm.TagConst != "" {
			tagged = append(tagged, arm)
		}
	}
	if len(tagged) == 0 {
		return
	}
	e.printf("// The %s values of %s's arms.\nconst (\n", def.Discriminant, def.GoName)
	for _, arm := range tagged {
		e.printf("\t%s = %q\n", arm.TagConst, arm.Tag)
	}
	e.printf(")\n\n")
}

func (e *emitter) unionMarshal(def *Def) {
	e.printf("// marshal%s writes the arm together with the discriminant the union owns.\n", def.Ident)
	e.printf("func marshal%s(value %s) ([]byte, error) {\n", def.Ident, def.GoName)
	e.printf("\tswitch value := value.(type) {\n")
	for _, arm := range def.Arms {
		e.printf("\tcase *%s:\n", arm.GoType)
		e.rejectNilUnionArm(def, arm)
		if arm.TagConst != "" {
			e.printf("\t\treturn wire.TagObject(%q, %s, value)\n", def.Discriminant, arm.TagConst)
		} else {
			e.printf("\t\treturn json.Marshal(value)\n")
		}
	}
	e.unionMarshalDefaults(def)
}

func (e *emitter) unionMarshalDefaults(def *Def) {
	e.printf("\tcase nil:\n\t\treturn nil, errors.New(\"acp: %s is required and none was set\")\n", def.GoName)
	e.printf("\tdefault:\n\t\treturn nil, fmt.Errorf(\"acp: %%T is not an arm of %s\", value)\n", def.GoName)
	e.printf("\t}\n}\n\n")
}

// An interface holding a nil arm is not nil in Go, but it is still no value on
// the wire. Rejecting it in every generated union makes the invariant hold for
// all of them rather than for the ones a caller happens to inspect.
func (e *emitter) rejectNilUnionArm(union *Def, arm *Arm) {
	e.printf("\t\tif value == nil {\n")
	e.printf("\t\t\treturn nil, errors.New(\"acp: %s holds a nil *%s arm\")\n", union.GoName, arm.GoType)
	e.printf("\t\t}\n")
}

func (e *emitter) unionUnmarshal(def *Def) {
	var tagged, untagged []*Arm
	var catchAll, absent *Arm
	for _, arm := range def.Arms {
		switch {
		case arm.CatchAll:
			catchAll = arm
		case arm.TagConst != "":
			tagged = append(tagged, arm)
		case arm.Untagged:
			absent = arm
		default:
			untagged = append(untagged, arm)
		}
	}
	discriminated := len(tagged) > 0 || catchAll != nil || absent != nil

	e.printf("// unmarshal%s selects the arm the value belongs to.\n", def.Ident)
	e.printf("func unmarshal%s(data json.RawMessage) (%s, error) {\n", def.Ident, def.GoName)
	// Every arm of an object union is an object, so a value that is not one is
	// refused once, up front.
	if discriminated {
		e.printf("\tobject, objectErr := wire.DecodeObject(data)\n")
	} else {
		e.printf("\t_, objectErr := wire.DecodeObject(data)\n")
	}
	e.printf("\tif objectErr != nil {\n\t\treturn nil, objectErr\n\t}\n")

	if len(untagged) > 0 {
		e.overlappingUnionArms(def, len(tagged) > 0, catchAll != nil || absent != nil)
		e.printf("}\n\n")
		return
	}
	if !discriminated {
		e.printf("\n\treturn nil, errors.New(\"acp: no %s arm matches this value\")\n}\n\n", def.GoName)
		return
	}

	e.printf("\n\ttag, ok := wire.Tag(object, %q)\n", def.Discriminant)
	e.printf("\tif !ok {\n")
	if absent != nil {
		e.decodeArm(absent, "\t\t")
	} else {
		e.printf("\t\treturn nil, wire.At(%q, wire.ErrRequired)\n", def.Discriminant)
	}
	e.printf("\t}\n")
	if len(tagged) == 1 {
		// A one-case switch is a conditional spelled the long way.
		e.printf("\tif tag == %s {\n", tagged[0].TagConst)
		e.decodeArm(tagged[0], "\t\t")
		e.printf("\t}\n")
	} else if len(tagged) > 1 {
		e.printf("\tswitch tag {\n")
		for _, arm := range tagged {
			e.printf("\tcase %s:\n", arm.TagConst)
			e.decodeArm(arm, "\t\t")
		}
		e.printf("\t}\n")
	}
	if catchAll != nil {
		e.printf("%s", comment("\t", []string{
			"A discriminant no known arm claims, which is the catch-all's `not` clause.",
		}))
		e.decodeArm(catchAll, "\t")
		e.printf("}\n\n")
		return
	}
	e.printf("\treturn nil, fmt.Errorf(\"acp: %%q is not a %s the schema defines\", tag)\n}\n\n", def.GoName)
}

func (e *emitter) decodeArm(arm *Arm, indent string) {
	e.printf("%svalue, err := wire.UnmarshalValue[%s](data)\n", indent, arm.GoType)
	e.printf("%sif err != nil {\n%s\treturn nil, err\n%s}\n", indent, indent, indent)
	e.printf("%sreturn &value, nil\n", indent)
}

// Untagged anyOf alternatives can match even when a known tag's payload is
// malformed. Schema order resolves overlapping successes; catch-all arms still
// enforce their own exclusions rather than absorbing malformed known arms.
func (e *emitter) overlappingUnionArms(def *Def, tagged, guarded bool) {
	if tagged {
		e.printf("\ttag, hasTag := wire.Tag(object, %q)\n", def.Discriminant)
	} else if guarded {
		e.printf("\t_, hasTag := wire.Tag(object, %q)\n", def.Discriminant)
	}
	for _, arm := range def.Arms {
		indent := "\t"
		switch {
		case arm.TagConst != "":
			e.printf("\n\tif hasTag && tag == %s {\n", arm.TagConst)
			indent += "\t"
		case arm.CatchAll:
			e.printf("\n\tif hasTag {\n")
			indent += "\t"
		case arm.Untagged:
			e.printf("\n\tif !hasTag {\n")
			indent += "\t"
		default:
			e.printf("\n")
		}
		e.printf("%sif value, err := wire.UnmarshalValue[%s](data); err == nil {\n", indent, arm.GoType)
		e.printf("%s\treturn &value, nil\n%s}\n", indent, indent)
		if arm.TagConst != "" || arm.CatchAll || arm.Untagged {
			e.printf("\t}\n")
		}
	}
	e.printf("\n\treturn nil, errors.New(\"acp: no %s arm matches this value\")\n", def.GoName)
}

// catchAllValidate enforces the catch-all arm's `not` clause in both directions,
// and the alternatives the arm is itself a union of.
func (e *emitter) catchAllValidate(def *Def) {
	catchAll := def.CatchAll
	doc := []string{
		"validate enforces the `not` clause of the catch-all arm: the discriminant",
		"values the known arms claim are reserved, and a value carrying one is a",
		"malformed known arm rather than a custom one.",
	}
	if len(catchAll.Alternatives) > 0 {
		doc = append(doc, "",
			"It also enforces the alternatives this arm is itself a union of: their",
			"properties are retained rather than declared, so each candidate's codec",
			"runs over "+retainedField+".")
	}
	e.printf("%s", comment("", doc))

	tagField := goName(catchAll.TagProperty)
	e.printf("func (x *%s) validate() error {\n", def.GoName)
	e.printf("\tif slices.Contains([]string{%s}, x.%s) {\n", quotedStrings(catchAll.ReservedTags), tagField)
	e.printf("\t\treturn wire.At(%q, fmt.Errorf(\n", catchAll.TagProperty)
	e.printf("\t\t\t\"acp: %%q is reserved by a known arm, but the value does not match that arm\", x.%s))\n",
		tagField)
	e.printf("\t}\n")

	if len(catchAll.Alternatives) > 0 {
		decoders := make([]string, len(catchAll.Alternatives))
		wanted := make([]string, len(catchAll.Alternatives))
		for i, alternative := range catchAll.Alternatives {
			decoders[i] = fmt.Sprintf(
				"\n\t\tfunc(raw []byte) error { var v %s; return v.UnmarshalJSON(raw) },", alternative.GoName)
			wanted[i] = "{" + quotedStrings(alternative.Required) + "}"
		}
		e.printf("\tif !wire.SatisfiesAnyAlternative(x.%s,%s\n\t) {\n", retainedField, strings.Join(decoders, ""))
		e.printf("\t\treturn fmt.Errorf(\n")
		e.printf("\t\t\t\"acp: a %s decodes as none of the alternatives its arm is a union of: %%v\",\n", def.GoName)
		e.printf("\t\t\t[][]string{%s})\n", strings.Join(wanted, ", "))
		e.printf("\t}\n")
	}
	e.printf("\treturn nil\n}\n\n")
}
