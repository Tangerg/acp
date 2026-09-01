package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// A Plan is everything the emitter needs, with every schema question already
// answered. Keeping the two apart is what makes the hard part — deciding what a
// schema construct becomes in Go — reviewable on its own, and it is where the
// generator refuses a construct it does not implement rather than emitting
// something that merely compiles.
type Plan struct {
	Defs      []*Def   // in emission order
	Closure   []string // schema definition names, sorted
	SchemaTag string
}

type defKind int

const (
	kindStruct defKind = iota
	// kindNewtype is a definition that is one Go scalar or one Go container under
	// a name of its own: SessionId, ProtocolVersion, and the arms of a value
	// union. What the underlying type is does not change how it is emitted.
	kindNewtype
	kindStringUnion
	kindNumberUnion
	kindObjectUnion
	kindValueUnion
	// kindNullArm is the null arm of a value union, which needs a Go type so that
	// it can implement the union's interface but has no value to carry.
	kindNullArm
	// kindRawValue is a definition the schema constrains not at all: the payload of
	// an extension method, which is whatever its sender and receiver agreed on.
	kindRawValue
)

// A Def is one generated top-level type.
type Def struct {
	SchemaName string // "" for a generated union arm that no definition backs
	GoName     string
	// Ident is the exported spelling of GoName, which the generated helper
	// functions and interface methods are named after. For an unexported type the
	// two differ: the type is requestID and its selector is unmarshalRequestID,
	// because isrequestID would be neither readable nor conventional.
	Ident string
	// Description is the schema's prose, unrendered. A union's doc comment is
	// built rather than quoted — it has to name its arms — so it needs the prose
	// without the symbol prefix Doc already carries.
	Description string
	Doc         []string
	Kind        defKind
	Unstable    bool

	// kindStruct.
	Fields []*Field
	// Embeds is the payload type a generated union arm carries, or empty.
	Embeds string
	// Retained, for a catch-all union arm, holds the properties the arm's own
	// schema does not declare. The schema gives such an arm additionalProperties,
	// so those keys are a vendor's payload and have to survive.
	Retained bool
	// ReservedTags are the discriminant values known arms claim. A catch-all arm
	// whose tag is one of them is the malformed known arm the `not` clause
	// excludes, not a custom one.
	ReservedTags []string
	TagProperty  string

	// kindNewtype, kindNumberUnion, kindNullArm.
	GoBase string

	// kindStringUnion.
	Values []*EnumValue
	Closed bool

	// kindObjectUnion, kindValueUnion.
	Discriminant string
	Arms         []*Arm
	Open         bool

	// Flattened is the Go type of the union a struct carries in the same JSON
	// object as its own properties, or empty. The schema does this where a type
	// has common properties and one of several kind-specific shapes.
	Flattened string
	// FlattenedIdent is that union's exported spelling, for its selector's name.
	FlattenedIdent string
}

// An EnumValue is one constant arm of a string union.
type EnumValue struct {
	GoName string
	Value  string
	Doc    []string
}

// An Arm is one arm of a union.
type Arm struct {
	Tag      string // "" when the arm has no discriminant constant
	GoType   string
	CatchAll bool
	// IsNull marks the null arm of a value union, which is selected before any
	// other because every other arm's decoder refuses null.
	IsNull bool
}

// A Field is one property of a generated struct.
type Field struct {
	JSONName string
	GoName   string
	Doc      []string
	GoType   string

	Required bool
	Nullable bool
	Opt      bool // the Go type is Opt[...], so absent and null stay apart

	Value    *ValueType
	Skip     bool // x-deserialize-skip-invalid-items
	Fallback fallbackKind
	// DefaultLit is the Go literal the fbDefault fallback assigns.
	DefaultLit string
}

// A fallbackKind is what a property marked x-deserialize-default-on-error
// becomes when its value does not decode. The four cases are the reference
// implementation's, which derives the fallback from the schema rather than
// naming it: the declared default if there is one, an empty array for an array
// that cannot be null, and otherwise nothing at all.
type fallbackKind int

const (
	fbNone fallbackKind = iota
	fbAbsent
	fbEmptySlice
	fbDefault
)

// The Go types the schema's scalar formats map to. They are named rather than
// spelled out because three passes have to agree on them: the type expression,
// the fallback literal and the bound check that proves the type enforces the
// schema's own range.
// jsonString is JSON Schema's name for the string type, which three passes
// compare against.
const jsonString = "string"

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

type valueKind int

const (
	vScalar valueKind = iota
	vRef
	vSlice
	vMap
	vMeta
	// vAny is a property the schema constrains not at all, which is a JSON value
	// of any shape. Error.data is the only one, and it is deliberate: the
	// specification says an error may carry whatever context its sender has.
	vAny
)

// A ValueType is a property's type with nullability stripped off, because
// nullability is a property of the property and not of the type it carries.
type ValueType struct {
	Kind valueKind
	Go   string // the Go type expression
	// Ident is the exported spelling of Go, which the generated selector for a
	// union is named after. It differs from Go only for an unexported type.
	Ident   string
	Ref     string // schema definition name, for vRef
	IsUnion bool   // a vRef whose definition is a union with a generated selector
	Elem    *ValueType
}

// A Manifest is the committed root set. Generation scope is its transitive $ref
// closure.
type Manifest struct {
	Payloads []string `json:"payloads"`
	Plumbing []string `json:"plumbing"`
	Markers  []string `json:"markers"`
	// Internal names the definitions that are generated but not exported, each
	// with the reason. JSON-RPC plumbing is in the closure because a payload
	// reaches it, not because a caller ever names it.
	Internal map[string]string `json:"internal"`
	// Projections are the handle-facing params types: a wire request minus the
	// identifiers a handle owns. Raw so that the bucket can carry its own
	// $comment, as every other bucket does.
	Projections map[string]json.RawMessage `json:"projections"`
	Probes      map[string]string          `json:"probes"`
}

// ProjectionSpecs decodes the projection bucket, skipping the comment key.
func (m *Manifest) ProjectionSpecs() (map[string]*ProjectionSpec, error) {
	specs := make(map[string]*ProjectionSpec, len(m.Projections))
	for _, name := range sortedKeys(m.Projections) {
		if strings.HasPrefix(name, "$") {
			continue
		}
		spec := new(ProjectionSpec)
		if err := json.Unmarshal(m.Projections[name], spec); err != nil {
			return nil, fmt.Errorf("projection %s: %w", name, err)
		}
		specs[name] = spec
	}
	return specs, nil
}

// A ProjectionSpec derives one params type from one request type.
type ProjectionSpec struct {
	From string `json:"from"`
	// Without names the properties the handle supplies, which the projection
	// therefore does not carry.
	Without []string `json:"without"`
	Why     string   `json:"why"`
}

// Roots returns every root the manifest names, sorted. A projection's source is a
// root too: there is nothing to project from otherwise.
func (m *Manifest) Roots() []string {
	roots := slices.Concat(m.Payloads, m.Plumbing, m.Markers)
	specs, err := m.ProjectionSpecs()
	if err == nil {
		for _, spec := range specs {
			roots = append(roots, spec.From)
		}
	}
	roots = append(roots, definitionKeys(m.Internal)...)
	roots = append(roots, definitionKeys(m.Probes)...)
	slices.Sort(roots)
	return slices.Compact(roots)
}

// definitionKeys drops the comment key a JSON object uses to explain itself.
func definitionKeys(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		if strings.HasPrefix(name, "$") {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

type planner struct {
	doc   *Document
	names *names

	// internal is the definitions the manifest says are generated but not
	// exported: JSON-RPC plumbing a caller never names.
	internal map[string]bool

	goNames map[string]string // schema definition name -> Go type name
	idents  map[string]string // schema definition name -> exported spelling
	// armPayloads counts, for every definition, how many union arms across the
	// whole schema use it as their sole payload. A payload used by exactly one arm
	// can be that arm's Go type; one used by several cannot, because the arms
	// would be indistinguishable.
	armPayloads map[string]int
	// armGoTypes is the Go type of each arm, keyed by union name and arm index,
	// allocated over the whole schema so that a name does not depend on how much
	// of it the manifest reaches.
	armGoTypes map[armKey]string
	// wrappers are the arm types no definition backs, keyed by Go name.
	wrappers map[string]*Def

	planned map[string]*Def
	order   []string
}

type armKey struct {
	union string
	index int
}

func newPlanner(doc *Document) *planner {
	return &planner{
		doc:         doc,
		names:       newNames(),
		internal:    make(map[string]bool),
		goNames:     make(map[string]string),
		idents:      make(map[string]string),
		armPayloads: make(map[string]int),
		armGoTypes:  make(map[armKey]string),
		wrappers:    make(map[string]*Def),
		planned:     make(map[string]*Def),
	}
}

// definitionNames returns every definition name, sorted.
func (p *planner) definitionNames() []string {
	return sortedKeys(p.doc.Defs)
}

// allocate assigns Go names over the whole schema — every definition and every
// union arm, whether the manifest reaches it or not.
func (p *planner) allocate() error {
	for _, name := range p.definitionNames() {
		ident := goName(name)
		goTypeName := ident
		if p.internal[name] {
			goTypeName = unexport(ident)
		}
		if err := p.names.claim(goTypeName, "#/$defs/"+name); err != nil {
			return err
		}
		p.goNames[name] = goTypeName
		p.idents[name] = ident
	}

	// Count sole-payload uses before naming any arm: whether an arm can be its
	// payload's own type depends on how many other arms want the same payload.
	for _, name := range p.definitionNames() {
		for _, arm := range p.doc.Defs[name].Arms() {
			if payload := armPayload(arm); payload != "" {
				p.armPayloads[payload]++
			}
		}
	}

	for _, name := range p.definitionNames() {
		if err := p.allocateArmsOf(name); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) allocateArmsOf(name string) error {
	def := p.doc.Defs[name]
	arms := def.Arms()
	switch {
	case isFlattenedUnion(def):
		flattened := p.idents[name] + flattenedSuffix
		if p.internal[name] {
			flattened = unexport(flattened)
		}
		if err := p.names.claim(flattened, "#/$defs/"+name+" flattened union"); err != nil {
			return err
		}
		return p.allocateObjectArms(name, arms)
	case isObjectUnion(def):
		return p.allocateObjectArms(name, arms)
	case isValueUnion(def):
		for i, arm := range arms {
			goType := p.idents[name] + valueArmName(arm)
			if p.internal[name] {
				goType = unexport(goType)
			}
			if err := p.names.claim(goType, fmt.Sprintf("#/$defs/%s arm %d", name, i)); err != nil {
				return err
			}
			p.armGoTypes[armKey{name, i}] = goType
		}
		return nil
	default:
		return nil
	}
}

func (p *planner) allocateObjectArms(name string, arms []*Schema) error {
	for i, arm := range arms {
		goType, err := p.allocateArm(name, i, arm)
		if err != nil {
			return err
		}
		p.armGoTypes[armKey{name, i}] = goType
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
	return "Null"
}

// isFlattenedUnion reports a definition that is an object and a union at once:
// properties of its own, plus a choice of kind-specific shapes in the same JSON
// object.
func isFlattenedUnion(schema *Schema) bool {
	return len(schema.Properties) > 0 && len(schema.Arms()) > 0
}

// isValueUnion reports a union whose arms are different JSON shapes rather than
// different values of one shape. It is the residual case: an object union, a
// string enumeration and an integer enumeration are all recognised first.
func isValueUnion(schema *Schema) bool {
	arms := schema.Arms()
	if len(arms) == 0 || isFlattenedUnion(schema) || isObjectUnion(schema) {
		return false
	}
	return !allArmsHaveBase(arms, jsonString) && !allArmsHaveBase(arms, "integer")
}

// allocateArm names one arm of an object union.
//
// The arm is its payload's own type when it has exactly one $ref payload, no
// properties of its own besides the discriminant, and no other arm anywhere in
// the schema shares that payload. That is what makes ContentBlock's arms
// TextContent and ImageContent rather than something invented. Otherwise the arm
// needs a type of its own, named after the arm — its title, or its discriminant
// value — and qualified by the union when that name is already taken.
func (p *planner) allocateArm(union string, index int, arm *Schema) (string, error) {
	owner := fmt.Sprintf("#/$defs/%s arm %d", union, index)
	payload := armPayload(arm)
	if !isCatchAll(arm) && payload != "" && p.armPayloads[payload] == 1 {
		return p.goNames[payload], nil
	}

	preferred := ""
	switch {
	case arm.Title != "":
		preferred = goName(arm.Title)
	default:
		if tag := armTag(arm); tag != "" {
			preferred = goName(tag)
		}
	}
	if preferred == "" {
		return "", fmt.Errorf("%s: cannot name an arm with neither a title nor a discriminant constant", owner)
	}

	// An arm with no payload of its own has no domain name to inherit: it is not
	// a thing, it is this union's cancelled case, or its catch-all. Naming it
	// after the union says so, and keeps a word as general as Cancelled out of
	// the package's namespace, where a later schema release could want it.
	if payload == "" {
		qualified := p.goNames[union] + preferred
		if err := p.names.claim(qualified, owner); err != nil {
			return "", err
		}
		return qualified, nil
	}
	return p.names.claimArm(preferred, p.goNames[union], owner)
}

// closure returns the transitive $ref closure of the manifest's roots.
func (p *planner) closure(roots []string) ([]string, error) {
	seen := make(map[string]bool)
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		name := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[name] {
			continue
		}
		def, ok := p.doc.Defs[name]
		if !ok {
			return nil, fmt.Errorf("the manifest names %q, which the schema does not define", name)
		}
		seen[name] = true
		queue = append(queue, refNames(def)...)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Plan resolves the manifest into everything the emitter needs.
func (p *planner) Plan(manifest *Manifest, schemaTag string) (*Plan, error) {
	for _, name := range definitionKeys(manifest.Internal) {
		p.internal[name] = true
	}
	if err := p.allocate(); err != nil {
		return nil, err
	}
	closure, err := p.closure(manifest.Roots())
	if err != nil {
		return nil, err
	}
	for _, name := range closure {
		if _, err := p.plan(name); err != nil {
			return nil, fmt.Errorf("#/$defs/%s: %w", name, err)
		}
	}

	specs, specErr := manifest.ProjectionSpecs()
	if specErr != nil {
		return nil, specErr
	}
	for _, name := range sortedKeys(specs) {
		if err := p.planProjection(name, specs[name]); err != nil {
			return nil, fmt.Errorf("projection %s: %w", name, err)
		}
	}

	defs := make([]*Def, 0, len(p.order))
	for _, goTypeName := range p.order {
		if def, ok := p.planned[goTypeName]; ok {
			defs = append(defs, def)
			continue
		}
		defs = append(defs, p.wrappers[goTypeName])
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].GoName < defs[j].GoName })
	if err := p.checkVisibility(defs); err != nil {
		return nil, err
	}
	return &Plan{Defs: defs, Closure: closure, SchemaTag: schemaTag}, nil
}

// planProjection derives a handle-facing params type from a wire request type.
//
// The projection is generated rather than written, which is the whole point: it is
// the wire type minus the identifiers the handle owns, so every other property —
// including _meta — survives, and the identifier exists exactly once and cannot
// disagree with itself. A hand-written params type would be a lossy summary of the
// wire, and would go stale the next time the schema adds a property.
func (p *planner) planProjection(name string, spec *ProjectionSpec) error {
	if spec.Why == "" {
		return errors.New("no stated reason; a projection has to say which handle owns the identifiers")
	}
	source, err := p.plan(spec.From)
	if err != nil {
		return err
	}
	if source.Kind != kindStruct {
		return fmt.Errorf("%s is not a struct, so there is nothing to project", spec.From)
	}
	if err := p.names.claim(name, "projection of #/$defs/"+spec.From); err != nil {
		return err
	}

	def := &Def{
		GoName:    name,
		Ident:     name,
		Kind:      kindStruct,
		Unstable:  source.Unstable,
		Retained:  source.Retained,
		Flattened: source.Flattened,
		Doc: append([]string{
			name + " — the parameters of " + spec.From + " that a caller supplies.",
			"",
		}, wrap(76, spec.Why+" It is "+spec.From+" without "+
			joinQuoted(spec.Without)+", which the handle owns, so every other property survives — "+
			"including _meta.")...),
	}
	def.FlattenedIdent = source.FlattenedIdent

	dropped := make(map[string]bool, len(spec.Without))
	for _, property := range spec.Without {
		dropped[property] = true
	}
	for _, field := range source.Fields {
		if dropped[field.JSONName] {
			delete(dropped, field.JSONName)
			continue
		}
		def.Fields = append(def.Fields, field)
	}
	if len(dropped) > 0 {
		return fmt.Errorf("%s has no %s property to leave out", spec.From, joinQuoted(sortedKeys(dropped)))
	}

	p.wrappers[name] = def
	p.order = append(p.order, name)
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

// checkVisibility refuses an exported type that names an unexported one.
//
// Such a type compiles and is unusable: an importer can hold the value but cannot
// write the field's type, construct one, or switch on it. A published type whose
// field a caller cannot name is worse than no type at all, so the manifest's
// internal list has to be closed under what reaches it.
func (p *planner) checkVisibility(defs []*Def) error {
	unexported := make(map[string]bool)
	for _, def := range defs {
		if def.GoName != def.Ident {
			unexported[def.GoName] = true
		}
	}
	for _, def := range defs {
		if def.GoName != def.Ident {
			continue
		}
		for _, field := range def.Fields {
			for _, named := range []string{field.Value.Go, field.GoType} {
				if unexported[named] {
					return fmt.Errorf(
						"%s is exported and its %s property is the unexported %s; "+
							"either export that definition or add %s to the manifest's internal list",
						def.GoName, field.JSONName, named, def.SchemaName)
				}
			}
		}
		for _, arm := range def.Arms {
			if unexported[arm.GoType] {
				return fmt.Errorf("%s is exported and its %s arm is unexported", def.GoName, arm.GoType)
			}
		}
	}
	return nil
}

func (p *planner) plan(name string) (*Def, error) {
	if def, ok := p.planned[p.goNames[name]]; ok && def.SchemaName == name {
		return def, nil
	}
	schema := p.doc.Defs[name]
	def := &Def{
		SchemaName:  name,
		GoName:      p.goNames[name],
		Ident:       p.idents[name],
		Description: schema.Description,
		Unstable:    isUnstable(schema.Description),
	}
	def.Doc = docComment(def.GoName, schema.Description)
	p.planned[def.GoName] = def
	p.order = append(p.order, def.GoName)

	arms := schema.Arms()
	switch {
	case isObjectUnion(schema):
		if err := p.planObjectUnion(def, schema); err != nil {
			return nil, err
		}
	case len(arms) > 0 && allArmsHaveBase(arms, jsonString):
		p.planStringUnion(def, arms)
	case len(arms) > 0 && allArmsHaveBase(arms, "integer"):
		if err := p.planNumberUnion(def, arms); err != nil {
			return nil, err
		}
	case schema.Type.Base() == "object" || len(schema.Properties) > 0:
		if err := p.planStruct(def, schema); err != nil {
			return nil, err
		}
		if len(arms) > 0 {
			if err := p.planFlattenedUnion(def, schema); err != nil {
				return nil, err
			}
		}
	case schema.Type.Base() == jsonString:
		def.Kind = kindNewtype
		def.GoBase = goString
	case schema.Type.Base() == "integer" || schema.Type.Base() == "number":
		base, err := goNumericType(schema)
		if err != nil {
			return nil, err
		}
		def.Kind = kindNewtype
		def.GoBase = base
	case len(arms) > 0:
		// Everything left is a union whose arms are different JSON shapes rather
		// than different values of one shape.
		if err := p.planValueUnion(def, arms); err != nil {
			return nil, err
		}
	case isUnconstrained(schema):
		def.Kind = kindRawValue
	default:
		return nil, fmt.Errorf("unimplemented definition shape: keywords %v", schema.Keywords)
	}
	return def, nil
}

func (p *planner) planStruct(def *Def, schema *Schema) error {
	def.Kind = kindStruct
	required := schema.Required
	for _, jsonName := range schema.PropertyOrder {
		field, err := p.planField(jsonName, schema.Properties[jsonName], slices.Contains(required, jsonName))
		if err != nil {
			return fmt.Errorf("property %q: %w", jsonName, err)
		}
		def.Fields = append(def.Fields, field)
	}
	return nil
}

func (p *planner) planField(jsonName string, schema *Schema, required bool) (*Field, error) {
	value, err := p.valueType(schema)
	if err != nil {
		return nil, err
	}
	field := &Field{
		JSONName: jsonName,
		GoName:   goName(jsonName),
		Doc:      docComment("", schema.Description),
		Required: required,
		// A property the schema does not constrain admits null along with
		// everything else, so it is nullable however the schema spells it — which
		// is not at all.
		Nullable: schema.Nullable() || value.Kind == vAny,
		Value:    value,
		Skip:     schema.SkipInvalidItems,
	}
	if field.Skip && value.Kind != vSlice {
		return nil, errors.New("x-deserialize-skip-invalid-items on a property that is not an array")
	}

	hasDefault := len(schema.Default) > 0
	switch {
	case field.Nullable:
		field.Opt = true
	case field.Required, value.Kind == vSlice, hasDefault:
		// A required property always has a value; an optional array uses nil for
		// absent, which a slice can express; an optional property with a declared
		// default takes that default when it is absent.
	default:
		field.Opt = true
	}
	if field.Opt {
		field.GoType = "Opt[" + value.Go + "]"
	} else {
		field.GoType = value.Go
	}

	// A declared default is what an absent property becomes, whether or not the
	// property also recovers from a malformed one, so the literal is resolved
	// from the presence of `default` alone.
	if hasDefault {
		literal, err := p.defaultLiteral(value, schema.Default)
		if err != nil {
			return nil, err
		}
		field.DefaultLit = literal
	}
	if err := p.planFallback(field, schema, hasDefault); err != nil {
		return nil, err
	}
	return field, nil
}

func (p *planner) planFallback(field *Field, schema *Schema, hasDefault bool) error {
	switch {
	case !schema.DefaultOnError:
		field.Fallback = fbNone
	case hasDefault:
		field.Fallback = fbDefault
	// An array recovers to an empty array only when it cannot be null. The
	// reference implementation reaches the same place by a different route — its
	// array test requires `type` to be the single name "array", which a nullable
	// array's ["array","null"] fails — and the two rules agree on every property
	// in the schema. Stating it as nullability is the honest version of the same
	// rule: an array that may be null has somewhere else to go.
	case field.Value.Kind == vSlice && !field.Nullable:
		field.Fallback = fbEmptySlice
	case field.Opt:
		field.Fallback = fbAbsent
	default:
		return fmt.Errorf(
			"x-deserialize-default-on-error with no declared default on a required %s, which has no absent "+
				"state to recover into", field.GoType)
	}
	return nil
}

func (p *planner) valueType(schema *Schema) (*ValueType, error) {
	if ref := schema.SoleRef(); ref != "" {
		target, ok := p.doc.Defs[ref]
		if !ok {
			return nil, fmt.Errorf("$ref to %q, which the schema does not define", ref)
		}
		def, err := p.plan(ref)
		if err != nil {
			return nil, fmt.Errorf("#/$defs/%s: %w", ref, err)
		}
		// A union of either kind needs the generated selector: a Go interface
		// cannot decode into itself, and its arms are not distinguishable to
		// json.Unmarshal. A flattened union is not one of these — it is a struct,
		// and the struct's own codec reaches the union it carries.
		isUnion := isObjectUnion(target) || isValueUnion(target)
		return &ValueType{Kind: vRef, Go: def.GoName, Ident: def.Ident, Ref: ref, IsUnion: isUnion}, nil
	}

	switch base := schema.Type.Base(); base {
	case jsonString:
		return &ValueType{Kind: vScalar, Go: goString}, nil
	case "boolean":
		return &ValueType{Kind: vScalar, Go: goBool}, nil
	case "integer", "number":
		goType, err := goNumericType(schema)
		if err != nil {
			return nil, err
		}
		return &ValueType{Kind: vScalar, Go: goType}, nil
	case "array":
		if schema.Items == nil {
			return nil, errors.New("an array with no items schema")
		}
		elem, err := p.valueType(schema.Items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		return &ValueType{Kind: vSlice, Go: "[]" + elem.Go, Elem: elem}, nil
	case "object":
		if len(schema.Properties) > 0 {
			return nil, errors.New("an inline object is not implemented; the schema names its object types")
		}
		if string(schema.AdditionalProperties) == "true" {
			return &ValueType{Kind: vMeta, Go: "Meta"}, nil
		}
		// additionalProperties carrying a schema is a map, and its own type is the
		// value type. There is no key type to resolve: JSON object keys are
		// strings.
		if len(schema.AdditionalProperties) > 0 {
			var entry Schema
			if err := entry.UnmarshalJSON(schema.AdditionalProperties); err != nil {
				return nil, fmt.Errorf("additionalProperties: %w", err)
			}
			elem, err := p.valueType(&entry)
			if err != nil {
				return nil, fmt.Errorf("additionalProperties: %w", err)
			}
			return &ValueType{Kind: vMap, Go: "map[string]" + elem.Go, Elem: elem}, nil
		}
		return nil, errors.New("an inline object is not implemented; the schema names its object types")
	case "":
		if isUnconstrained(schema) {
			return &ValueType{Kind: vAny, Go: "json.RawMessage"}, nil
		}
		return nil, fmt.Errorf("unimplemented property shape: keywords %v", schema.Keywords)
	default:
		return nil, fmt.Errorf("unimplemented property shape: keywords %v", schema.Keywords)
	}
}

// isUnconstrained reports a schema that says nothing about the value at all. Only
// the keywords that constrain one count: a description and a deserialisation
// extension leave every JSON value admissible.
func isUnconstrained(schema *Schema) bool {
	for _, keyword := range schema.Keywords {
		switch keyword {
		case "description", "title", "x-deserialize-default-on-error", "x-docs-ignore",
			"x-method", "x-side":
			continue
		default:
			return false
		}
	}
	return true
}

func (p *planner) planStringUnion(def *Def, arms []*Schema) {
	def.Kind = kindStringUnion
	def.Closed = true
	for _, arm := range arms {
		var value string
		if len(arm.Const) == 0 {
			// The bare-typed arm. Its presence is how the schema says the union is
			// open, so a value outside the list is valid and is kept as received.
			def.Closed = false
			continue
		}
		_ = json.Unmarshal(arm.Const, &value)
		def.Values = append(def.Values, &EnumValue{
			GoName: def.GoName + goName(value),
			Value:  value,
			Doc:    docComment(def.GoName+goName(value), arm.Description),
		})
	}
}

// planNumberUnion is planStringUnion for an integer-valued enumeration. ErrorCode
// is the only one, and it is open: eight predefined codes plus an unrestricted
// int32 arm, so an unknown in-range code is valid and must survive.
func (p *planner) planNumberUnion(def *Def, arms []*Schema) error {
	def.Kind = kindNumberUnion
	def.Closed = true
	for _, arm := range arms {
		goType, err := goNumericType(arm)
		if err != nil {
			return err
		}
		if def.GoBase == "" {
			def.GoBase = goType
		}
		if def.GoBase != goType {
			return fmt.Errorf("arms disagree about the numeric format: %s and %s", def.GoBase, goType)
		}
		if len(arm.Const) == 0 {
			def.Closed = false
			continue
		}
		var value json.Number
		if decodeErr := json.Unmarshal(arm.Const, &value); decodeErr != nil {
			return fmt.Errorf("a constant arm that is not a number: %s", arm.Const)
		}
		name, nameErr := p.armConstantName(def, arm, value.String())
		if nameErr != nil {
			return nameErr
		}
		def.Values = append(def.Values, &EnumValue{
			GoName: name,
			Value:  value.String(),
			Doc:    docComment(name, arm.Description),
		})
	}
	return nil
}

// armConstantName names a constant arm. A string arm's own value reads well as a
// name; an integer arm's does not, so the schema's title for it is used instead —
// which is why ErrorCode's arms are named after "Parse error" rather than after
// -32700.
func (p *planner) armConstantName(def *Def, arm *Schema, value string) (string, error) {
	if arm.Title == "" {
		return "", fmt.Errorf("a constant arm with the value %s and no title to name it after", value)
	}
	return def.GoName + goName(arm.Title), nil
}

// planValueUnion plans a union whose arms are different JSON shapes: RequestId is
// null, an integer or a string, and SessionConfigSelectOptions is one of two
// differently-typed arrays.
//
// An arm is a Go type of its own, because only a named type can implement the
// union's interface, and it is named after the union as well as after itself. An
// object union's arms are named after domain concepts — user_message_chunk — and
// stand alone; a value union's titles are shape words like Ungrouped, Str and
// Number, which would be poor names in a package of their own.
// planFlattenedUnion plans the union a struct carries in its own JSON object.
//
// The union is a type of its own — a Go struct cannot be several shapes at once —
// and the struct holds it in a field. The name is the struct's plus Value, which
// is generated rather than taken from the schema because the schema gives this
// construct no name at all: it is an object and a oneOf side by side.
func (p *planner) planFlattenedUnion(def *Def, schema *Schema) error {
	ident := def.Ident + flattenedSuffix
	goType := ident
	if p.internal[def.SchemaName] {
		goType = unexport(ident)
	}
	for _, field := range def.Fields {
		if field.GoName == flattenedSuffix {
			return fmt.Errorf("the type already declares a %s property, so its flattened union has no name",
				field.JSONName)
		}
	}

	union := &Def{
		SchemaName:  def.SchemaName,
		GoName:      goType,
		Ident:       ident,
		Description: schema.Description,
		Unstable:    def.Unstable,
	}
	if err := p.planObjectUnion(union, schema); err != nil {
		return err
	}
	p.wrappers[goType] = union
	p.order = append(p.order, goType)
	def.Flattened = goType
	def.FlattenedIdent = ident
	return nil
}

// flattenedSuffix names both the generated union type and the field that holds
// it, so that one construct has one word for it.
const flattenedSuffix = "Value"

func (p *planner) planValueUnion(def *Def, arms []*Schema) error {
	def.Kind = kindValueUnion
	for i, arm := range arms {
		goType := p.armGoTypes[armKey{def.SchemaName, i}]
		if arm.Type.Base() == "" && arm.Type.Has("null") {
			p.wrappers[goType] = &Def{
				GoName: goType,
				Ident:  goName(goType),
				Kind:   kindNullArm,
				Doc:    valueArmDoc(goType, def.GoName, arm.Description),
			}
			p.order = append(p.order, goType)
			def.Arms = append(def.Arms, &Arm{GoType: goType, IsNull: true})
			continue
		}
		base, err := p.valueType(arm)
		if err != nil {
			return fmt.Errorf("arm %d: %w", i, err)
		}
		p.wrappers[goType] = &Def{
			GoName: goType,
			Ident:  goName(goType),
			Kind:   kindNewtype,
			GoBase: base.Go,
			Doc:    valueArmDoc(goType, def.GoName, arm.Description),
		}
		p.order = append(p.order, goType)
		def.Arms = append(def.Arms, &Arm{GoType: goType})
	}
	return nil
}

func valueArmDoc(goType, union, description string) []string {
	doc := []string{goType + " — one arm of " + union + "."}
	if described := docComment("", description); len(described) > 0 {
		doc = append(doc, "")
		doc = append(doc, described...)
	}
	return doc
}

func (p *planner) planObjectUnion(def *Def, schema *Schema) error {
	def.Kind = kindObjectUnion
	arms := schema.Arms()

	// A union without a discriminant is an ordinary anyOf: its arms are tried in
	// schema order and the first that decodes wins. EmbeddedResourceResource is
	// one, and its two arms are told apart by which property they require.
	def.Discriminant = discriminant(schema, arms)

	var reserved []string
	for _, arm := range arms {
		if tag := armTag(arm); tag != "" {
			reserved = append(reserved, tag)
		}
	}

	for i, arm := range arms {
		goType := p.armGoTypes[armKey{def.SchemaName, i}]
		planned := &Arm{Tag: armTag(arm), GoType: goType, CatchAll: isCatchAll(arm)}
		def.Arms = append(def.Arms, planned)
		if planned.CatchAll {
			def.Open = true
		}

		payload := armPayload(arm)
		switch {
		case planned.CatchAll:
			wrapper, err := p.planCatchAllArm(goType, arm, def, reserved)
			if err != nil {
				return fmt.Errorf("arm %d: %w", i, err)
			}
			p.wrappers[goType] = wrapper
			p.order = append(p.order, goType)
		case payload != "" && goType == p.goNames[payload]:
			target, err := p.plan(payload)
			if err != nil {
				return fmt.Errorf("arm %d: #/$defs/%s: %w", i, payload, err)
			}
			if err := p.checkArmPayload(target, def.Discriminant); err != nil {
				return fmt.Errorf("arm %d: %w", i, err)
			}
		default:
			wrapper, err := p.planWrapperArm(goType, arm, def, payload)
			if err != nil {
				return fmt.Errorf("arm %d: %w", i, err)
			}
			p.wrappers[goType] = wrapper
			p.order = append(p.order, goType)
		}
	}
	return nil
}

// planWrapperArm plans an arm that needs a type of its own.
//
// Two cases reach here. An arm whose payload another arm also uses cannot be that
// payload — SessionUpdate has three arms carrying a ContentChunk, and only the
// discriminant tells them apart. And an arm with no payload at all carries
// nothing but its discriminant, so there is no existing type to be.
func (p *planner) planWrapperArm(goType string, arm *Schema, union *Def, payload string) (*Def, error) {
	def := &Def{
		GoName:   goType,
		Ident:    goName(goType),
		Kind:     kindStruct,
		Unstable: isUnstable(arm.Description),
	}

	doc := []string{goType + " — one arm of " + union.GoName + "."}
	if described := docComment("", arm.Description); len(described) > 0 {
		doc = append(doc, "")
		doc = append(doc, described...)
	}

	for _, jsonName := range arm.PropertyOrder {
		if jsonName == union.Discriminant {
			continue // the union writes and reads its own discriminant
		}
		field, err := p.planField(jsonName, arm.Properties[jsonName], slices.Contains(arm.Required, jsonName))
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", jsonName, err)
		}
		def.Fields = append(def.Fields, field)
	}

	if payload != "" {
		target, err := p.plan(payload)
		if err != nil {
			return nil, fmt.Errorf("#/$defs/%s: %w", payload, err)
		}
		if err := p.checkArmPayload(target, union.Discriminant); err != nil {
			return nil, err
		}
		if len(def.Fields) > 0 {
			return nil, fmt.Errorf(
				"an arm with both a %s payload and properties of its own is not implemented", target.GoName)
		}
		def.Embeds = target.GoName
		doc = append(doc, "")
		doc = append(doc, wrap(76, "It carries a "+target.GoName+
			" and exists as a type of its own because more than one arm of "+union.GoName+
			" carries one: the discriminant is the only thing that tells them apart.")...)
	}

	def.Doc = doc
	return def, nil
}

// checkArmPayload refuses a payload that declares the union's discriminant.
//
// The discriminant is written by the union rather than by the arm, by splicing
// it into the arm's encoded object. That is safe without re-parsing the arm's
// output only if the arm cannot have written the same key itself, so the
// generator checks here instead of the encoder checking on every message.
func (p *planner) checkArmPayload(payload *Def, discriminantName string) error {
	if discriminantName == "" {
		return nil
	}
	for _, field := range payload.Fields {
		if field.JSONName == discriminantName {
			return fmt.Errorf("payload %s declares the discriminant property %q", payload.GoName, discriminantName)
		}
	}
	return nil
}

func (p *planner) planCatchAllArm(goType string, arm *Schema, union *Def, reserved []string) (*Def, error) {
	doc := []string{goType + " — the open catch-all arm of " + union.GoName + "."}
	if described := docComment("", arm.Description); len(described) > 0 {
		doc = append(doc, "")
		doc = append(doc, described...)
	}
	doc = append(doc, "")
	doc = append(doc, wrap(76, fmt.Sprintf(
		"The schema's `not` clause reserves the discriminant values %s's known arms claim, so a value carrying "+
			"one of those is a malformed known arm rather than a custom one and does not land here. Every "+
			"property this arm does not declare is kept in Extra, because the schema gives the arm "+
			"additionalProperties: those keys are the sender's payload.", union.GoName))...)

	def := &Def{
		GoName:       goType,
		Ident:        goName(goType),
		Kind:         kindStruct,
		Retained:     true,
		ReservedTags: reserved,
		TagProperty:  union.Discriminant,
		Doc:          doc,
	}
	for _, jsonName := range arm.PropertyOrder {
		field, err := p.planField(jsonName, arm.Properties[jsonName], slices.Contains(arm.Required, jsonName))
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", jsonName, err)
		}
		def.Fields = append(def.Fields, field)
	}
	if len(def.Fields) == 0 {
		return nil, errors.New("a catch-all arm with no declared discriminant property")
	}
	return def, nil
}

// isObjectUnion reports whether a definition is a union whose arms are objects.
func isObjectUnion(schema *Schema) bool {
	arms := schema.Arms()
	if len(arms) == 0 || len(schema.Properties) > 0 {
		return false
	}
	for _, arm := range arms {
		if arm.Type.Has("object") {
			return true
		}
		if arm.Type == nil && armPayload(arm) != "" {
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
// declares nothing but the discriminant itself.
func armPayload(arm *Schema) string {
	if len(arm.AllOf) != 1 {
		return ""
	}
	name := arm.AllOf[0].RefName()
	if name == "" {
		return ""
	}
	for _, schema := range arm.Properties {
		if len(schema.Const) == 0 {
			return "" // a property of the arm's own, beyond the discriminant
		}
	}
	return name
}

func armTag(arm *Schema) string {
	for _, schema := range arm.Properties {
		if len(schema.Const) == 0 {
			continue
		}
		var tag string
		if err := json.Unmarshal(schema.Const, &tag); err == nil {
			return tag
		}
	}
	return ""
}

// isCatchAll reports the open arm: the one whose `not` clause excludes the known
// arms' discriminant values. Exactly four unions in the v1 schema have one, and
// its presence — not the choice of oneOf or anyOf — is what makes a union open.
func isCatchAll(arm *Schema) bool {
	return arm.Not != nil
}

// discriminant is the property that selects an arm: the declared discriminator,
// or the property the arms pin to a constant.
func discriminant(schema *Schema, arms []*Schema) string {
	if schema.Discriminator != nil {
		return schema.Discriminator.PropertyName
	}
	for _, arm := range arms {
		names := make([]string, 0, len(arm.Properties))
		for name, property := range arm.Properties {
			if len(property.Const) > 0 {
				names = append(names, name)
			}
		}
		if len(names) == 1 {
			return names[0]
		}
	}
	return ""
}

// goNumericType picks the Go type from the schema's format, which also disposes
// of the numeric bounds: every `minimum` in the schema is 0 on an unsigned
// format and the single `maximum` is 65535 on uint16, so the Go type enforces
// them and a generated range check would be unreachable code.
func goNumericType(schema *Schema) (string, error) {
	goType, ok := map[string]string{
		"int32":  goInt32,
		"int64":  goInt64,
		"uint16": goUint16,
		"uint32": goUint32,
		"uint64": goUint64,
		"double": goFloat64,
	}[schema.Format]
	if !ok {
		return "", fmt.Errorf("numeric type with unimplemented format %q", schema.Format)
	}
	if err := checkBoundsAreFree(goType, schema); err != nil {
		return "", err
	}
	return goType, nil
}

func checkBoundsAreFree(goType string, schema *Schema) error {
	free := map[string][2]float64{
		goInt32:   {-2147483648, 2147483647},
		goInt64:   {-9223372036854775808, 9223372036854775807},
		goUint16:  {0, 65535},
		goUint32:  {0, 4294967295},
		goUint64:  {0, 18446744073709551615},
		goFloat64: {},
	}[goType]
	if schema.Minimum != nil && *schema.Minimum != free[0] {
		return fmt.Errorf("minimum %v is not the low bound of %s, and a generated range check is not implemented", *schema.Minimum, goType)
	}
	if schema.Maximum != nil && *schema.Maximum != free[1] {
		return fmt.Errorf("maximum %v is not the high bound of %s, and a generated range check is not implemented", *schema.Maximum, goType)
	}
	return nil
}

// defaultLiteral renders the schema's declared default as a Go literal.
//
// A literal rather than a package-level value decoded from the JSON at start-up:
// the compiler then checks that the default fits the type it defaults, and a
// default that stopped fitting after a schema bump is a build failure rather than
// a silent zero value.
//
// The recursion mirrors the reference implementation's, which hands the declared
// value straight back without re-parsing it — so a property the default does not
// mention keeps its Go zero value, which is the absent state, and not that
// property's own default.
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
		return scalarLiteral(def.GoName, raw)
	case kindStruct:
		return p.structDefaultLiteral(def, raw)
	default:
		return "", fmt.Errorf("a declared default on a %s is not implemented", def.GoName)
	}
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
	if string(raw) == "null" {
		return "OptNull[" + field.Value.Go + "]()", nil
	}
	inner, err := p.defaultLiteral(field.Value, raw)
	if err != nil {
		return "", err
	}
	return "OptValue(" + inner + ")", nil
}

func scalarLiteral(goType string, raw json.RawMessage) (string, error) {
	switch goType {
	case goBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a boolean: %w", raw, err)
		}
		return strconv.FormatBool(v), nil
	case goString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a string: %w", raw, err)
		}
		return strconv.Quote(v), nil
	case goInt32, goInt64, goUint16, goUint32, goUint64, goFloat64:
		var v json.Number
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("default %s is not a number: %w", raw, err)
		}
		return goType + "(" + v.String() + ")", nil
	default:
		// A named string or numeric type: the literal is a conversion of the
		// underlying one, and which underlying one it is does not matter here.
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

// refNames returns every definition a schema node references, transitively
// within the node.
func refNames(schema *Schema) []string {
	var names []string
	var walk func(*Schema)
	walk = func(node *Schema) {
		if node == nil {
			return
		}
		if name := node.RefName(); name != "" {
			names = append(names, name)
		}
		for _, child := range node.Properties {
			walk(child)
		}
		walk(node.Items)
		walk(node.Not)
		for _, list := range [][]*Schema{node.AllOf, node.AnyOf, node.OneOf} {
			for _, child := range list {
				walk(child)
			}
		}
	}
	walk(schema)
	slices.Sort(names)
	return slices.Compact(names)
}

// isUnstable reports upstream's UNSTABLE marker, which the doc comment repeats
// verbatim and the module's compatibility promise carves out.
func isUnstable(description string) bool {
	return strings.Contains(description, "**UNSTABLE**")
}
