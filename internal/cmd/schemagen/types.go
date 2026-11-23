package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// The Go types the schema's scalar formats map to, and the bounds each one
// enforces by itself.
const (
	goBool    = "bool"
	goString  = "string"
	goInt32   = "int32"
	goInt64   = "int64"
	goUint16  = "uint16"
	goUint32  = "uint32"
	goUint64  = "uint64"
	goFloat64 = "float64"
)

// A numericFormat is one schema `format` for a number: the JSON type it belongs
// to, the Go type that carries it, and that Go type's own range. Every pass that
// needs one of these facts reads it from this table.
type numericFormat struct {
	jsonType string
	goType   string
	// low and high are the Go type's bounds, which a schema minimum or maximum
	// must equal for the type alone to enforce it. Empty for a float.
	low, high string
}

var numericFormats = map[string]numericFormat{
	"int32":  {jsonInteger, goInt32, "-2147483648", "2147483647"},
	"int64":  {jsonInteger, goInt64, "-9223372036854775808", "9223372036854775807"},
	"uint16": {jsonInteger, goUint16, "0", "65535"},
	"uint32": {jsonInteger, goUint32, "0", "4294967295"},
	"uint64": {jsonInteger, goUint64, "0", "18446744073709551615"},
	"double": {jsonNumber, goFloat64, "", ""},
}

// isNumericGoType reports a Go type some numeric format maps to.
func isNumericGoType(goType string) bool {
	for _, format := range numericFormats {
		if format.goType == goType {
			return true
		}
	}
	return false
}

// formatURI is the one string format this generator enforces: the URL a client
// sends a user to.
const formatURI = "uri"

// flattenedSuffix names both the generated union type and the field that holds
// it, so that one construct has one word for it.
const flattenedSuffix = "Value"

// planStruct plans an object's properties, leaving out the one a flattened union
// owns as its discriminant.
func (p *planner) planStruct(def *Def, schema *Schema, unionOwned string) error {
	def.Kind = kindStruct
	if err := refuseUnconsumed(schema, "a struct"); err != nil {
		return err
	}
	if schema.AdditionalProperties != nil && !schema.AdditionalProperties.IsTrue() {
		return errors.New("additionalProperties other than true on a struct is not enforced, so it is not implemented")
	}
	for _, jsonName := range schema.PropertyOrder {
		if jsonName == unionOwned {
			continue
		}
		field, err := p.planField(jsonName, schema.Properties[jsonName], slices.Contains(schema.Required, jsonName))
		if err != nil {
			return fmt.Errorf("property %q: %w", jsonName, err)
		}
		def.Fields = append(def.Fields, field)
	}
	return nil
}

// refuseUnconsumed refuses the keywords no planning step consumes in this
// position. A known keyword can still be meaningless where it appears, and the
// generator would otherwise produce a type that silently does not enforce it.
func refuseUnconsumed(schema *Schema, position string) error {
	switch {
	case len(schema.Const) > 0:
		return fmt.Errorf("const on %s is not enforced, so it is not implemented", position)
	case schema.Not != nil:
		return fmt.Errorf("not on %s is not enforced, so it is not implemented", position)
	case schema.UnevaluatedProperties != nil:
		return fmt.Errorf("unevaluatedProperties on %s is not implemented", position)
	default:
		return nil
	}
}

func (p *planner) planField(jsonName string, schema *Schema, required bool) (*Field, error) {
	if err := refuseUnconsumed(schema, "a property"); err != nil {
		return nil, err
	}
	value, err := p.valueType(schema)
	if err != nil {
		return nil, err
	}
	if routingErr := checkUnionIsRouted(value); routingErr != nil {
		return nil, routingErr
	}
	field := &Field{
		JSONName: jsonName,
		GoName:   goName(jsonName),
		Doc:      docComment("", schema.Description),
		Required: required,
		// A property the schema does not constrain admits null along with
		// everything else.
		Nullable:      schema.Nullable() || value.Kind == vAny,
		Value:         value,
		Skip:          schema.SkipInvalidItems,
		DefaultOnNull: schema.DefaultOnNull,
	}
	if field.Skip && value.Kind != vSlice {
		return nil, errors.New("x-deserialize-skip-invalid-items on a property that is not an array")
	}
	if field.DefaultOnNull && (value.Kind != vSlice && value.Kind != vMap || field.Nullable) {
		return nil, errors.New("x-deserialize-default-on-null requires a non-null array or map schema")
	}

	hasDefault := len(schema.Default) > 0
	switch {
	case field.Nullable:
		field.Opt = true
	case field.Required, value.Kind == vSlice, hasDefault:
		// A required property always has a value; an optional array uses nil for
		// absent; an optional property with a declared default takes that default.
	default:
		field.Opt = true
	}

	if hasDefault {
		literal, literalErr := p.defaultLiteral(value, schema.Default)
		if literalErr != nil {
			return nil, literalErr
		}
		field.DefaultLit = literal
	}
	if fallbackErr := planFallback(field, schema, hasDefault); fallbackErr != nil {
		return nil, fallbackErr
	}
	return field, nil
}

func planFallback(field *Field, schema *Schema, hasDefault bool) error {
	switch {
	case !schema.DefaultOnError:
		field.Fallback = fbNone
	case hasDefault:
		field.Fallback = fbDefault
	// An array recovers to an empty array only when it cannot be null: an array
	// that may be null has somewhere else to go. The reference implementation
	// reaches the same answer for every property in the schema.
	case field.Value.Kind == vSlice && !field.Nullable:
		field.Fallback = fbEmptySlice
	case field.Opt:
		field.Fallback = fbAbsent
	default:
		return fmt.Errorf(
			"x-deserialize-default-on-error with no declared default on a required %s, which has no absent "+
				"state to recover into", field.Value.Go)
	}
	return nil
}

func (p *planner) valueType(schema *Schema) (*ValueType, error) {
	if schema.MinLength != nil && schema.Type.Base() != jsonString {
		return nil, errors.New("minLength requires a string schema")
	}
	if ref := schema.SoleRef(); ref != "" {
		return p.refValueType(ref)
	}

	switch base := schema.Type.Base(); base {
	case jsonString:
		return stringValueType(schema)
	case jsonBoolean:
		return &ValueType{Kind: vScalar, Go: goBool}, nil
	case jsonInteger, jsonNumber:
		goType, err := goNumericType(schema)
		if err != nil {
			return nil, err
		}
		return &ValueType{Kind: vScalar, Go: goType}, nil
	case jsonArray:
		if schema.Items == nil {
			return nil, errors.New("an array with no items schema")
		}
		elem, err := p.valueType(schema.Items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		return &ValueType{Kind: vSlice, Go: "[]" + elem.Go, Elem: elem}, nil
	case jsonObject:
		return p.objectValueType(schema)
	case "":
		if schema.isUnconstrained() {
			return &ValueType{Kind: vAny, Go: "json.RawMessage"}, nil
		}
		return nil, fmt.Errorf("unimplemented property shape: keywords %v", schema.Keywords)
	default:
		return nil, fmt.Errorf("unimplemented property shape: keywords %v", schema.Keywords)
	}
}

func (p *planner) refValueType(ref string) (*ValueType, error) {
	if binding := p.bindings[ref]; binding != nil {
		if err := p.checkBinding(ref, binding); err != nil {
			return nil, err
		}
		return &ValueType{Kind: vBound, Go: binding.GoType}, nil
	}
	if _, ok := p.doc.Defs[ref]; !ok {
		return nil, fmt.Errorf("$ref to %q, which the schema does not define", ref)
	}
	def, err := p.plan(ref)
	if err != nil {
		return nil, fmt.Errorf("%s%s: %w", definitionPointer, ref, err)
	}
	shape, err := p.shape(ref)
	if err != nil {
		return nil, err
	}
	return &ValueType{Kind: vRef, Go: def.GoName, Ident: def.Ident, Ref: ref, IsUnion: shape.isUnion()}, nil
}

// checkBinding holds a bound definition to the shape its Go type owns, so a
// schema release that changes the grammar stops generation instead of being
// decoded by a type written for the old one.
func (p *planner) checkBinding(name string, binding *Binding) error {
	schema := p.doc.Defs[name]
	if schema == nil {
		return fmt.Errorf("binding %s names a definition the schema does not have", name)
	}
	switch binding.Shape {
	case shapeJSONRPCID:
		var hasNull, hasInt64, hasString bool
		for _, arm := range schema.Arms() {
			switch {
			case arm.Type.Base() == "" && arm.Type.Has(jsonNull):
				hasNull = true
			case arm.Type.Base() == jsonInteger && arm.Format == "int64":
				hasInt64 = true
			case arm.Type.Base() == jsonString && arm.Format == "":
				hasString = true
			default:
				return fmt.Errorf("binding %s: an arm with keywords %v is not part of a JSON-RPC id", name, arm.Keywords)
			}
		}
		if !hasNull || !hasInt64 || !hasString || len(schema.Arms()) != 3 {
			return fmt.Errorf("binding %s is no longer null, an int64 or a string", name)
		}
		return nil
	default:
		return fmt.Errorf("binding %s declares the unimplemented shape %q", name, binding.Shape)
	}
}

func (p *planner) objectValueType(schema *Schema) (*ValueType, error) {
	if len(schema.Properties) > 0 {
		return nil, errors.New("an inline object is not implemented; the schema names its object types")
	}
	switch {
	case schema.AdditionalProperties.IsTrue():
		return &ValueType{Kind: vMeta, Go: "Meta"}, nil
	case schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil:
		// additionalProperties carrying a schema is a map whose values have that
		// type. JSON object keys are strings, so there is no key type to resolve.
		elem, err := p.valueType(schema.AdditionalProperties.Schema)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		return &ValueType{Kind: vMap, Go: "map[string]" + elem.Go, Elem: elem}, nil
	default:
		return nil, errors.New("an inline object is not implemented; the schema names its object types")
	}
}

func stringValueType(schema *Schema) (*ValueType, error) {
	value := &ValueType{Kind: vScalar, Go: goString}
	if schema.MinLength != nil {
		value.MinLength = *schema.MinLength
	}
	switch schema.Format {
	case "":
		return value, nil
	case formatURI:
		value.Format = formatURI
		return value, nil
	default:
		return nil, fmt.Errorf("unimplemented string format %q", schema.Format)
	}
}

// goNumericType picks the Go type from the schema's format, which also disposes
// of the numeric bounds: a bound equal to the Go type's own range needs no
// generated check.
func goNumericType(schema *Schema) (string, error) {
	format, ok := numericFormats[schema.Format]
	if !ok {
		return "", fmt.Errorf("numeric type with unimplemented format %q", schema.Format)
	}
	if base := schema.Type.Base(); base != format.jsonType {
		return "", fmt.Errorf("format %q belongs to %s, not to %s", schema.Format, format.jsonType, base)
	}
	if format.low == "" && (schema.Minimum != nil || schema.Maximum != nil) {
		return "", fmt.Errorf("a bound on %s is not implemented", format.goType)
	}
	if schema.Minimum != nil && !equalNumber(*schema.Minimum, format.low) {
		return "", fmt.Errorf("minimum %v is not the low bound of %s, and a generated range check is not implemented",
			*schema.Minimum, format.goType)
	}
	if schema.Maximum != nil && !equalNumber(*schema.Maximum, format.high) {
		return "", fmt.Errorf("maximum %v is not the high bound of %s, and a generated range check is not implemented",
			*schema.Maximum, format.goType)
	}
	return format.goType, nil
}

func equalNumber(number json.Number, literal string) bool {
	value, valid := new(big.Rat).SetString(string(number))
	want, defined := new(big.Rat).SetString(literal)
	return valid && defined && value.Cmp(want) == 0
}

// checkUnionIsRouted refuses a property whose union would not reach the generated
// selector that encodes and decodes it.
//
// A union's discriminant belongs to the union rather than to the arm, and a Go
// interface cannot decode into itself, so every union has to reach a generated
// function. The emitter knows three positions: the property itself, one element
// deep in an array, and one value deep in an object. A union deeper than that
// would be encoded without its discriminant, which the peer cannot read.
func checkUnionIsRouted(value *ValueType) error {
	if value == nil || value.IsUnion {
		return nil
	}
	if value.Kind != vSlice && value.Kind != vMap {
		return nil
	}
	elem := value.Elem
	if elem == nil || elem.IsUnion {
		return nil
	}
	if containsUnion(elem) {
		return fmt.Errorf(
			"a union nested inside %s has no generated codec: it would be encoded without the "+
				"discriminant the union owns and could not be decoded at all", value.Go)
	}
	return nil
}

// containsUnion reports whether a union is reachable through containers. It does
// not follow a reference to a named type, whose own codec handles its unions.
func containsUnion(value *ValueType) bool {
	for value != nil {
		if value.IsUnion {
			return true
		}
		if value.Kind != vSlice && value.Kind != vMap {
			return false
		}
		value = value.Elem
	}
	return false
}

// defaultLiteral renders the schema's declared default as a Go literal, so the
// compiler checks that the default still fits its type after a schema bump.
//
// It mirrors the reference implementation, which hands the declared value back
// without re-parsing it: a property the default does not mention keeps its Go
// zero value, which is the absent state, not that property's own default.
func (p *planner) defaultLiteral(value *ValueType, raw json.RawMessage) (string, error) {
	switch value.Kind {
	case vScalar:
		return scalarLiteral(value.Go, raw)
	case vSlice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", fmt.Errorf("default %s is not an array: %w", raw, err)
		}
		parts := make([]string, len(items))
		for i, item := range items {
			part, err := p.defaultLiteral(value.Elem, item)
			if err != nil {
				return "", fmt.Errorf("default element %d: %w", i, err)
			}
			parts[i] = part
		}
		return value.Go + "{" + strings.Join(parts, ", ") + "}", nil
	case vMap:
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return "", fmt.Errorf("default %s is not an object: %w", raw, err)
		}
		parts := make([]string, 0, len(entries))
		for _, key := range sortedKeys(entries) {
			part, err := p.defaultLiteral(value.Elem, entries[key])
			if err != nil {
				return "", fmt.Errorf("default entry %q: %w", key, err)
			}
			parts = append(parts, strconv.Quote(key)+": "+part)
		}
		return value.Go + "{" + strings.Join(parts, ", ") + "}", nil
	case vRef:
		return p.refDefaultLiteral(value, raw)
	default:
		return "", fmt.Errorf("a declared default on a %s is not implemented", value.Go)
	}
}

func (p *planner) refDefaultLiteral(value *ValueType, raw json.RawMessage) (string, error) {
	def, err := p.plan(value.Ref)
	if err != nil {
		return "", err
	}
	switch def.Kind {
	case kindNewtype, kindStringUnion, kindNumberUnion:
		if literal, isConstant := def.constantFor(raw); isConstant {
			return literal, nil
		}
		return scalarLiteral(def.GoName, raw)
	case kindStruct:
		return p.structDefaultLiteral(def, raw)
	default:
		return "", fmt.Errorf("a declared default on a %s is not implemented", def.GoName)
	}
}

// constantFor names the generated constant whose value is raw, so a default that
// is one of an enumeration's values reads as that value's name.
func (d *Def) constantFor(raw json.RawMessage) (string, bool) {
	for _, value := range d.Values {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil && text == value.Value {
			return value.GoName, true
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err == nil && number.String() == value.Value {
			return value.GoName, true
		}
	}
	return "", false
}

func (p *planner) structDefaultLiteral(def *Def, raw json.RawMessage) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", fmt.Errorf("default %s is not an object: %w", raw, err)
	}

	fields := make(map[string]*Field, len(def.Fields))
	for _, field := range def.Fields {
		fields[field.JSONName] = field
	}
	for _, name := range sortedKeys(object) {
		if fields[name] == nil {
			return "", fmt.Errorf("the default for %s names %q, which the type does not declare", def.GoName, name)
		}
	}

	// Schema order, not the default's own, so that one type's literal reads the
	// same way wherever it is defaulted.
	var parts []string
	for _, field := range def.Fields {
		fieldRaw, declared := object[field.JSONName]
		if !declared {
			continue
		}
		part, err := p.fieldDefaultLiteral(field, fieldRaw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field.JSONName, err)
		}
		parts = append(parts, field.GoName+": "+part)
	}
	return def.GoName + "{" + strings.Join(parts, ", ") + "}", nil
}

func (p *planner) fieldDefaultLiteral(field *Field, raw json.RawMessage) (string, error) {
	if !field.Opt {
		return p.defaultLiteral(field.Value, raw)
	}
	if string(raw) == jsonNull {
		return "OptNull[" + field.Value.Go + "]()", nil
	}
	inner, err := p.defaultLiteral(field.Value, raw)
	if err != nil {
		return "", err
	}
	return "OptValue(" + inner + ")", nil
}

func scalarLiteral(goType string, raw json.RawMessage) (string, error) {
	switch {
	case goType == goBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a boolean: %w", raw, err)
		}
		return strconv.FormatBool(v), nil
	case goType == goString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a string: %w", raw, err)
		}
		return strconv.Quote(v), nil
	case isNumericGoType(goType):
		var v json.Number
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a number: %w", raw, err)
		}
		return goType + "(" + v.String() + ")", nil
	default:
		// A named string or numeric type: the literal is a conversion of the
		// underlying one.
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return goType + "(" + strconv.Quote(text) + ")", nil
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return "", fmt.Errorf("default %s is neither a string nor a number", raw)
		}
		return goType + "(" + number.String() + ")", nil
	}
}
