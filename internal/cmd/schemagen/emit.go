package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// emitter writes the generated package. It answers no schema questions: every
// decision is already in the [Plan], so this file is the mapping from a resolved
// decision to Go source.
type emitter struct {
	buf bytes.Buffer
}

// generatedImports maps every package name generated code may select from to its
// import path. The import list is derived from the selectors the parsed body
// actually uses, so prose copied into comments cannot add an import.
var generatedImports = map[string]string{
	"errors":  "errors",
	"fmt":     "fmt",
	"json":    "encoding/json",
	"jsonrpc": "github.com/Tangerg/acp/jsonrpc",
	"slices":  "slices",
	"wire":    "github.com/Tangerg/acp/internal/wire",
}

func emit(plan *Plan) ([]byte, error) {
	e := new(emitter)
	for _, def := range plan.Defs {
		e.definition(def)
	}
	header := comment("", []string{
		generatedHeader,
		"",
		"Source: schema/schema.json, pinned to upstream release " + plan.SchemaTag + ".",
		fmt.Sprintf("Scope: %d of the schema's definitions, reached from schema/manifest.json.", len(plan.Closure)),
		"",
		"The doc comments are the published specification's own prose. A change to",
		"that prose therefore arrives through a reviewed schema update rather than",
		"through hand edits to this file.",
	})
	return goFile(header, e.buf.Bytes())
}

// goFile assembles a formatted package acp file from a body, importing exactly
// the packages the body selects from.
func goFile(header string, body []byte) ([]byte, error) {
	imports, err := usedImports(body)
	if err != nil {
		return body, err
	}
	var out bytes.Buffer
	out.WriteString(header)
	out.WriteString("\npackage acp\n\n")
	if len(imports) > 0 {
		// The standard library first, then the module's own packages, which is
		// the grouping goimports enforces.
		out.WriteString("import (\n")
		separated := false
		for _, path := range imports {
			if !separated && strings.Contains(path, ".") {
				out.WriteString("\n")
				separated = true
			}
			fmt.Fprintf(&out, "\t%s\n", strconv.Quote(path))
		}
		out.WriteString(")\n\n")
	}
	out.Write(body)
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return out.Bytes(), fmt.Errorf("the generated source does not parse: %w", err)
	}
	return formatted, nil
}

func usedImports(body []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "", append([]byte("package acp\n"), body...),
		parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("the generated source does not parse: %w", err)
	}
	used := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, isSelector := node.(*ast.SelectorExpr); isSelector {
			if pkg, isIdent := selector.X.(*ast.Ident); isIdent {
				if path, known := generatedImports[pkg.Name]; known {
					used[path] = true
				}
			}
		}
		return true
	})
	paths := slices.Collect(maps.Keys(used))
	slices.SortFunc(paths, func(left, right string) int {
		// Standard-library paths have no dot in their first element.
		leftStd, rightStd := !strings.Contains(left, "."), !strings.Contains(right, ".")
		if leftStd != rightStd {
			if leftStd {
				return -1
			}
			return 1
		}
		return strings.Compare(left, right)
	})
	return paths, nil
}

func (e *emitter) printf(format string, args ...any) {
	fmt.Fprintf(&e.buf, format, args...)
}

func (e *emitter) definition(def *Def) {
	switch def.Kind {
	case kindStruct:
		e.structDef(def)
	case kindNewtype:
		e.newtype(def)
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
	default:
		panic(fmt.Sprintf("schemagen: no emitter for definition kind %d", def.Kind))
	}
}

// fieldType renders a property's Go type.
func fieldType(field *Field) string {
	if field.Opt {
		return "Opt[" + field.Value.Go + "]"
	}
	return field.Value.Go
}

// -- unconstrained values ----------------------------------------------------

// A definition the schema constrains not at all is the raw bytes under a name of
// its own. The codec is explicit because a named type does not inherit
// json.RawMessage's, and the bytes would otherwise encode as base64.
func (e *emitter) rawValue(def *Def) {
	e.printf("%stype %s json.RawMessage\n\n", comment("", def.Doc), def.GoName)

	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	e.printf("\tif len(x) == 0 {\n\t\treturn []byte(\"null\"), nil\n\t}\n")
	e.printf("\treturn x, nil\n}\n\n")

	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\t*x = append((*x)[:0], data...)\n\treturn nil\n}\n\n")
}

func (e *emitter) newtype(def *Def) {
	e.printf("%stype %s %s\n\n", comment("", def.Doc), def.GoName, def.Base.Go)
	e.namedValueUnmarshal(def, def.Base)
	if encode := encodeExpr(&Field{Value: def.Base, Required: true}); encode != "" {
		e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
		e.printf("\treturn %s\n}\n\n", strings.ReplaceAll(encode, "$", def.Base.Go+"(x)"))
	}
}

func (e *emitter) namedValueUnmarshal(def *Def, base *ValueType) {
	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	e.printf("\tvalue, err := %s\n", decodeExpr(&Field{Value: base}, "data"))
	e.printf("\tif err != nil {\n\t\treturn err\n\t}\n")
	e.printf("\t*x = %s(value)\n\treturn nil\n}\n\n", def.GoName)
}

// -- structs -----------------------------------------------------------------

func (e *emitter) structDef(def *Def) {
	if len(def.Fields) == 0 && def.Embeds == "" && def.Flattened == nil && def.CatchAll == nil {
		// A type the schema gives no properties is still an object on the wire.
		e.printf("%stype %s struct{}\n\n", comment("", def.Doc), def.GoName)
		e.structUnmarshal(def)
		e.structMarshal(def)
		e.projection(def)
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
		e.printf("%s\t%s %s\n", comment("\t", field.Doc), field.GoName, fieldType(field))
	}
	if def.Flattened != nil {
		if len(def.Fields) > 0 {
			e.printf("\n")
		}
		e.printf("%s", comment("\t", []string{
			flattenedSuffix + " is the kind-specific part of this type, which the schema puts in the",
			"same JSON object as the properties above. A Go struct cannot be several shapes",
			"at once, so it holds the choice here and the encoder flattens it back.",
		}))
		e.printf("\t%s %s\n", flattenedSuffix, def.Flattened.GoName)
	}
	if def.CatchAll != nil {
		e.printf("\n%s", comment("\t", []string{
			retainedField + " preserves the sender's extension payload. Declared properties belong",
			"to their typed fields, including common fields of the containing object;",
			"an " + retainedField + " entry cannot override one of those owners.",
		}))
		e.printf("\t%s map[string]json.RawMessage\n", retainedField)
	}
	e.printf("}\n\n")

	if def.Embeds != "" {
		// The embedded payload's codec is promoted: the planner refuses a wrapper
		// with properties of its own, so the payload is the whole of its JSON.
		return
	}
	e.structUnmarshal(def)
	e.structMarshal(def)
	if def.CatchAll != nil {
		e.catchAllValidate(def)
	}
	e.projection(def)
}

func (e *emitter) structUnmarshal(def *Def) {
	e.printf("func (x *%s) UnmarshalJSON(data []byte) error {\n", def.GoName)
	if len(def.Fields) > 0 || def.CatchAll != nil {
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
	if def.Flattened != nil {
		e.printf("\n\t{\n")
		e.printf("\t\tvalue, err := unmarshal%s(data)\n", def.Flattened.Ident)
		e.printf("\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n")
		e.printf("\t\tx.%s = value\n\t}\n", flattenedSuffix)
	}
	if def.CatchAll != nil {
		e.printf("\n\tx.%s = wire.Extra(object, %s)\n", retainedField, quotedStrings(def.CatchAll.ExcludedExtraKeys))
		e.printf("\n\treturn x.validate()\n}\n\n")
		return
	}
	e.printf("\n\treturn nil\n}\n\n")
}

func (e *emitter) fieldUnmarshal(field *Field) {
	e.printf("\n\tif raw, ok := object[%q]; ok {\n", field.JSONName)
	body := "\t\t"
	hasNullCase := field.DefaultOnNull || field.Opt && field.Nullable
	if hasNullCase {
		e.printf("\t\tif wire.IsNull(raw) {\n")
		if field.DefaultOnNull {
			e.printf("\t\t\t%s\n", assign(field, field.Value.Go+"{}"))
		} else {
			e.printf("\t\t\tx.%s = OptNull[%s]()\n", field.GoName, field.Value.Go)
		}
		e.printf("\t\t} else {\n")
		body = "\t\t\t"
	}

	e.printf("%svalue, err := %s\n", body, decodeExpr(field, "raw"))
	switch field.Fallback {
	case fbNone:
		e.printf("%sif err != nil {\n%s\treturn wire.At(%q, err)\n%s}\n", body, body, field.JSONName, body)
		e.printf("%s%s\n", body, assign(field, "value"))
	case fbAbsent:
		e.printf("%sif err == nil {\n%s\t%s\n%s}\n", body, body, assign(field, "value"), body)
	case fbDefault, fbEmptySlice:
		e.printf("%sif err != nil {\n%s\tvalue = %s\n%s}\n", body, body, fallbackLiteral(field), body)
		e.printf("%s%s\n", body, assign(field, "value"))
	default:
		panic(fmt.Sprintf("schemagen: no decoder for fallback %d", field.Fallback))
	}

	if hasNullCase {
		e.printf("\t\t}\n")
	}
	if absent := absentAssignment(field); absent != "" {
		e.printf("\t} else {\n\t\t%s\n\t}\n", absent)
		return
	}
	e.printf("\t}\n")
}

// assign stores a value of the property's value type in its field.
func assign(field *Field, value string) string {
	if field.Opt {
		return fmt.Sprintf("x.%s = OptValue(%s)", field.GoName, value)
	}
	return fmt.Sprintf("x.%s = %s", field.GoName, value)
}

// fallbackLiteral is what a malformed property recovers to, in its value type.
func fallbackLiteral(field *Field) string {
	if field.Fallback == fbDefault {
		return field.DefaultLit
	}
	return field.Value.Go + "{}"
}

// zeroLiteral matches a literal that is its type's zero value, which an absent
// property already holds after the struct is reset.
var zeroLiteral = regexp.MustCompile(`^(false|""|0|[A-Za-z_][A-Za-z0-9_]*\((""|0)\))$`)

// absentAssignment is what an omitted property becomes, or the empty string when
// the reset struct already holds it.
func absentAssignment(field *Field) string {
	switch {
	case field.Required:
		return fmt.Sprintf("return wire.At(%q, wire.ErrRequired)", field.JSONName)
	case field.DefaultLit == "", !field.Opt && zeroLiteral.MatchString(field.DefaultLit):
		return ""
	default:
		return assign(field, field.DefaultLit)
	}
}

// decodeExpr is the call that turns one property's raw bytes into its value.
func decodeExpr(field *Field, input string) string {
	value := field.Value
	switch {
	case value.Kind == vSlice:
		elem := value.Elem
		switch {
		case elem.IsUnion && field.Skip:
			return fmt.Sprintf("wire.UnmarshalSliceFuncSkippingInvalid(%s, unmarshal%s)", input, elem.Ident)
		case elem.IsUnion:
			return fmt.Sprintf("wire.UnmarshalSliceFunc(%s, unmarshal%s)", input, elem.Ident)
		case field.Skip:
			return fmt.Sprintf("wire.UnmarshalSliceSkippingInvalid[%s](%s)", elem.Go, input)
		default:
			return fmt.Sprintf("wire.UnmarshalSlice[%s](%s)", elem.Go, input)
		}
	// A map of unions needs the union's generated function for the same reason
	// an array of them does.
	case value.Kind == vMap && value.Elem.IsUnion:
		return fmt.Sprintf("wire.UnmarshalMapFunc(%s, unmarshal%s)", input, value.Elem.Ident)
	case value.Kind == vMap:
		return fmt.Sprintf("wire.UnmarshalMap[%s](%s)", value.Elem.Go, input)
	case value.IsUnion:
		return fmt.Sprintf("unmarshal%s(%s)", value.Ident, input)
	// A bound type owns its whole grammar, including whether null is a value.
	case value.Kind == vBound:
		return fmt.Sprintf("wire.UnmarshalOwned[%s](%s)", value.Go, input)
	}
	// A format is enforced where the value is read, so a malformed URI recovers
	// exactly as a malformed anything else does.
	if constraints := stringConstraints(value); constraints != "" {
		return fmt.Sprintf("%s.Decode(%s)", constraints, input)
	}
	return fmt.Sprintf("wire.UnmarshalValue[%s](%s)", value.Go, input)
}

func (e *emitter) structMarshal(def *Def) {
	e.printf("func (x %s) MarshalJSON() ([]byte, error) {\n", def.GoName)
	if def.CatchAll != nil {
		// x is a local and so addressable, which keeps validate on the pointer
		// receiver an arm of a union is used through.
		e.printf("\tif err := x.validate(); err != nil {\n\t\treturn nil, err\n\t}\n\n")
	}
	e.printf("\tvar writer wire.ObjectWriter\n")
	for _, field := range def.Fields {
		e.fieldMarshal(field)
	}
	if def.Flattened != nil {
		e.printf("\t{\n")
		e.printf("\t\traw, err := marshal%s(x.%s)\n", def.Flattened.Ident, flattenedSuffix)
		e.printf("\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
		e.printf("\t\twriter.Embed(raw)\n\t}\n")
	}
	if def.CatchAll != nil {
		e.printf("\twriter.SetExtra(x.%s, %s)\n", retainedField, quotedStrings(def.CatchAll.ExcludedExtraKeys))
	}
	e.printf("\n\treturn writer.Bytes()\n}\n\n")
}

func quotedStrings(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}

func (e *emitter) fieldMarshal(field *Field) {
	encode := encodeExpr(field)
	if encode == "" {
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
// value, or the empty string when the writer can encode the value itself. It
// cannot when a union owns a discriminant, or when a required array has to encode
// as [] where a nil slice would encode as null.
func encodeExpr(field *Field) string {
	value := field.Value
	switch {
	case value.Kind == vSlice && value.Elem.IsUnion:
		return fmt.Sprintf("wire.MarshalSliceFunc($, marshal%s)", value.Elem.Ident)
	case value.Kind == vSlice:
		return "wire.MarshalSlice($)"
	case value.Kind == vMap && value.Elem.IsUnion:
		return fmt.Sprintf("wire.MarshalMapFunc($, marshal%s)", value.Elem.Ident)
	case value.Kind == vMap:
		return "wire.MarshalMap($)"
	case value.IsUnion:
		return fmt.Sprintf("marshal%s($)", value.Ident)
	}
	if constraints := stringConstraints(value); constraints != "" {
		return constraints + ".Encode($)"
	}
	return ""
}

func stringConstraints(value *ValueType) string {
	var constraints []string
	if value.MinLength > 0 {
		constraints = append(constraints, fmt.Sprintf("MinLength: %d", value.MinLength))
	}
	if value.Format == formatURI {
		constraints = append(constraints, "AbsoluteURI: true")
	}
	if len(constraints) == 0 {
		return ""
	}
	return "wire.StringConstraints{" + strings.Join(constraints, ", ") + "}"
}

// projection emits the conversion from a handle-facing params type back to the
// wire request, given the identifiers the handle owns. A nil receiver is the
// zero params value.
func (e *emitter) projection(def *Def) {
	if def.Projection == nil {
		return
	}
	source := def.Projection.From
	params := make([]string, len(def.Projection.Bound))
	for i, field := range def.Projection.Bound {
		params[i] = unexport(field.GoName) + " " + fieldType(field)
	}
	e.printf("// bind is the %s these parameters describe once the handle supplies its identifiers.\n",
		source.GoName)
	e.printf("func (x *%s) bind(%s) *%s {\n", def.GoName, strings.Join(params, ", "), source.GoName)
	e.printf("\tif x == nil {\n\t\tx = new(%s)\n\t}\n", def.GoName)
	e.printf("\treturn &%s{\n", source.GoName)
	for _, field := range source.Fields {
		if slices.Contains(def.Projection.Bound, field) {
			e.printf("\t\t%s: %s,\n", field.GoName, unexport(field.GoName))
			continue
		}
		e.printf("\t\t%s: x.%s,\n", field.GoName, field.GoName)
	}
	if source.Flattened != nil {
		e.printf("\t\t%s: x.%s,\n", flattenedSuffix, flattenedSuffix)
	}
	if source.CatchAll != nil {
		e.printf("\t\t%s: x.%s,\n", retainedField, retainedField)
	}
	e.printf("\t}\n}\n\n")
}
