package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// A Schema is one JSON Schema node, holding every keyword the published Agent
// Client Protocol schema uses and nothing else. A keyword this struct does not
// name is a keyword the generator has never seen, and [checkKnownKeywords]
// reports it rather than letting it be silently ignored.
type Schema struct {
	Ref         string `json:"$ref"`
	Title       string `json:"title"`
	Description string `json:"description"`

	Type    TypeSet           `json:"type"`
	Format  string            `json:"format"`
	Const   json.RawMessage   `json:"const"`
	Enum    []json.RawMessage `json:"enum"`
	Default json.RawMessage   `json:"default"`
	Minimum *float64          `json:"minimum"`
	Maximum *float64          `json:"maximum"`

	Properties map[string]*Schema `json:"properties"`
	Required   []string           `json:"required"`
	Items      *Schema            `json:"items"`

	AllOf []*Schema `json:"allOf"`
	AnyOf []*Schema `json:"anyOf"`
	OneOf []*Schema `json:"oneOf"`
	Not   *Schema   `json:"not"`

	AdditionalProperties  json.RawMessage `json:"additionalProperties"`
	UnevaluatedProperties json.RawMessage `json:"unevaluatedProperties"`
	Discriminator         *struct {
		PropertyName string `json:"propertyName"`
	} `json:"discriminator"`

	// The schema's own deserialisation extensions. Neither has any meaning to a
	// plain JSON decoder, and between them they cover 413 property occurrences.
	DefaultOnError   bool `json:"x-deserialize-default-on-error"`
	SkipInvalidItems bool `json:"x-deserialize-skip-invalid-items"`

	Method     string `json:"x-method"`
	Side       string `json:"x-side"`
	DocsIgnore bool   `json:"x-docs-ignore"`

	// PropertyOrder is the order Properties appear in the document. Go maps do
	// not have one, and the generated field order, the encoded key order and the
	// golden fixtures all depend on it: a message should be readable against the
	// schema that defines it.
	PropertyOrder []string `json:"-"`

	// Keywords is every keyword this node actually carried, so an unrecognised
	// one can be reported with the node it was on.
	Keywords []string `json:"-"`
}

// knownKeywords is the closed set this generator implements. It is spelled out
// rather than derived from the struct tags because two fields deliberately have
// none: PropertyOrder and Keywords are derived, not decoded.
var knownKeywords = map[string]bool{
	"$ref": true, "title": true, "description": true,
	"type": true, "format": true, "const": true, "enum": true,
	"default": true, "minimum": true, "maximum": true,
	"properties": true, "required": true, "items": true,
	"allOf": true, "anyOf": true, "oneOf": true, "not": true,
	"additionalProperties": true, "unevaluatedProperties": true,
	"discriminator":                  true,
	"x-deserialize-default-on-error": true, "x-deserialize-skip-invalid-items": true,
	"x-method": true, "x-side": true, "x-docs-ignore": true,
}

func (s *Schema) UnmarshalJSON(data []byte) error {
	type plain Schema
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	top, err := objectKeys(data)
	if err != nil {
		return err
	}
	s.Keywords = top
	if raw, ok := rawProperty(data, "properties"); ok {
		order, err := objectKeys(raw)
		if err != nil {
			return err
		}
		s.PropertyOrder = order
	}
	return nil
}

// Nullable reports whether the schema permits null, either as a member of a type
// set or as an anyOf arm. The two spellings appear 357 times between them and
// mean the same thing.
func (s *Schema) Nullable() bool {
	if s.Type.Has("null") {
		return true
	}
	for _, arm := range s.AnyOf {
		if arm.Type.Has("null") {
			return true
		}
	}
	return false
}

// RefName is the definition name a $ref points at, or the empty string.
func (s *Schema) RefName() string {
	if s.Ref == "" {
		return ""
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(s.Ref, prefix) {
		return ""
	}
	return s.Ref[len(prefix):]
}

// SoleRef unwraps the two spellings the schema uses to say "this property is
// one of these definitions": a single-element allOf, and an anyOf of a $ref and
// null. Both are how a $ref carries a description of its own.
func (s *Schema) SoleRef() string {
	if name := s.RefName(); name != "" {
		return name
	}
	if len(s.AllOf) == 1 && len(s.AnyOf) == 0 && len(s.OneOf) == 0 {
		return s.AllOf[0].RefName()
	}
	var refs []string
	for _, arm := range s.AnyOf {
		if arm.Type.Has("null") && arm.RefName() == "" {
			continue
		}
		refs = append(refs, arm.RefName())
	}
	if len(refs) == 1 && refs[0] != "" && len(s.AllOf) == 0 && len(s.OneOf) == 0 {
		return refs[0]
	}
	return ""
}

// Arms returns the union arms, whichever keyword spells them.
//
// The keyword itself is not reported because it decides nothing: oneOf and anyOf
// are both used for closed unions and for open ones, and what makes a union open
// is a `not` catch-all arm or a bare-typed arm.
func (s *Schema) Arms() []*Schema {
	if len(s.OneOf) > 0 {
		return s.OneOf
	}
	return s.AnyOf
}

// A TypeSet is JSON Schema's `type`, which is either one name or a list of them.
type TypeSet []string

func (t *TypeSet) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*t = TypeSet{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf(`"type" is neither a name nor a list of names: %w`, err)
	}
	*t = many
	return nil
}

func (t TypeSet) Has(name string) bool {
	return slices.Contains(t, name)
}

// Base is the single type name that is not null, or the empty string when the
// set is empty or names more than one.
func (t TypeSet) Base() string {
	var base string
	for _, n := range t {
		if n == "null" {
			continue
		}
		if base != "" {
			return ""
		}
		base = n
	}
	return base
}

// Document is the whole schema file.
type Document struct {
	Defs map[string]*Schema `json:"$defs"`
}

func loadDocument(data []byte) (*Document, error) {
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Defs) == 0 {
		return nil, errors.New("the schema has no $defs")
	}
	return &doc, nil
}

// objectKeys returns a JSON object's keys in document order.
func objectKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("expected a property name, got %v", tok)
		}
		keys = append(keys, key)
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// skipValue consumes one complete JSON value, including a nested object or
// array, so that the next token is the following property name.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func rawProperty(data []byte, name string) (json.RawMessage, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, false
	}
	raw, ok := top[name]
	return raw, ok
}

// checkKnownKeywords walks every node reachable from the document and reports
// the first keyword the generator does not implement.
//
// The alternative is a generator that ignores what it does not understand, and a
// schema bump that adds a constraint would then silently produce types that do
// not enforce it. This is the difference between a generator that is behind and
// a generator that is wrong.
func checkKnownKeywords(raw []byte) error {
	var top struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	// The raw document's own definitions, not the parsed ones: the parse silently
	// drops what it does not name, and what it drops is exactly what this exists
	// to find.
	for _, name := range sortedKeys(top.Defs) {
		if err := checkNode("#/$defs/"+name, top.Defs[name]); err != nil {
			return err
		}
	}
	return nil
}

func checkNode(path string, node json.RawMessage) error {
	if kind(node) != '{' {
		return nil // a boolean or a scalar subschema has no keywords to check
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(node, &obj); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, key := range sortedKeys(obj) {
		if !knownKeywords[key] {
			return fmt.Errorf("%s: unimplemented schema keyword %q", path, key)
		}
		if err := checkChildren(path, key, obj[key]); err != nil {
			return err
		}
	}
	return nil
}

func checkChildren(path, key string, value json.RawMessage) error {
	switch key {
	case "properties":
		var props map[string]json.RawMessage
		if err := json.Unmarshal(value, &props); err != nil {
			return fmt.Errorf("%s/properties: %w", path, err)
		}
		for _, name := range sortedKeys(props) {
			if err := checkNode(path+"/properties/"+name, props[name]); err != nil {
				return err
			}
		}
	case "allOf", "anyOf", "oneOf":
		var list []json.RawMessage
		if err := json.Unmarshal(value, &list); err != nil {
			return fmt.Errorf("%s/%s: %w", path, key, err)
		}
		for i, arm := range list {
			if err := checkNode(fmt.Sprintf("%s/%s/%d", path, key, i), arm); err != nil {
				return err
			}
		}
	case "items", "not", "additionalProperties", "unevaluatedProperties":
		return checkNode(path+"/"+key, value)
	}
	return nil
}

// kind returns the first significant byte of a JSON value, which is enough to
// tell an object from anything else.
func kind(data []byte) byte {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

// sortedKeys keeps every pass over the schema deterministic. Go map iteration is
// not, and a generator whose output — or whose first reported error — depends on
// it cannot be checked by regenerating it.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
