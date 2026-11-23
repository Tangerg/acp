package main

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// A Plan is everything the emitter needs, with every schema question already
// answered. The planner is where a construct the generator does not implement is
// refused, rather than emitted as something that merely compiles.
type Plan struct {
	Defs []*Def // sorted by Go name
	// Closure is every schema definition the generated file represents, sorted.
	// It is derived from what was planned, so the count a header reports is the
	// count the file contains.
	Closure   []string
	SchemaTag string
}

// A Def is one generated top-level type: the names and prose every kind shares,
// and the body its kind owns. The body's concrete type is the kind, so a field
// one kind needs cannot be set on another.
type Def struct {
	SchemaName string // "" for a generated type no definition backs
	GoName     string
	// Ident is the exported spelling of GoName, which generated helper functions
	// and interface methods are named after.
	Ident string
	// Description is the schema's prose, unrendered. Doc is the rendered comment
	// of a type whose comment is quoted; a union builds its own from Description.
	Description string
	Doc         []string
	Body        defBody
}

// A defBody is what one kind of definition owns.
type defBody interface {
	// referencedTypes are the generated types the body names.
	referencedTypes() []string
}

// A structBody is an object with properties.
type structBody struct {
	Fields []*Field
	// Embeds is the payload type a generated union arm carries, or empty.
	Embeds string
	// CatchAll is set on the open arm of an object union.
	CatchAll *CatchAll
	// Flattened is the union a struct carries in its own JSON object, or nil.
	Flattened *Def
	// Projection is set on a handle-facing params type.
	Projection *Projection
}

// A newtypeBody is one Go scalar or container under a name of its own.
type newtypeBody struct {
	Base *ValueType
}

// An enumeration is the constants of a string or number union. Open keeps a value
// outside them rather than refusing it.
type enumeration struct {
	Values []*EnumValue
	Open   bool
}

type stringEnumBody struct {
	enumeration
}

type numberEnumBody struct {
	enumeration
	Base *ValueType
}

type objectUnionBody struct {
	Discriminant string
	Arms         []*Arm
	// Open is set when the union has a catch-all arm.
	Open bool
}

// A valueUnionBody is a union whose arms are different JSON shapes, so it has no
// discriminant.
type valueUnionBody struct {
	Arms []*Arm
}

// A nullArmBody is the null arm of a value union: a Go type so that it can
// implement the union's interface, with no value to carry.
type nullArmBody struct{}

// A rawValueBody is a definition the schema constrains not at all.
type rawValueBody struct{}

func (b *structBody) referencedTypes() []string {
	var named []string
	for _, field := range b.Fields {
		named = append(named, field.Value.namedTypes()...)
	}
	if b.Embeds != "" {
		named = append(named, b.Embeds)
	}
	if b.Flattened != nil {
		named = append(named, b.Flattened.GoName)
	}
	return named
}

func (b *newtypeBody) referencedTypes() []string     { return b.Base.namedTypes() }
func (*stringEnumBody) referencedTypes() []string    { return nil }
func (b *numberEnumBody) referencedTypes() []string  { return b.Base.namedTypes() }
func (b *objectUnionBody) referencedTypes() []string { return armTypes(b.Arms) }
func (b *valueUnionBody) referencedTypes() []string  { return armTypes(b.Arms) }
func (nullArmBody) referencedTypes() []string        { return nil }
func (rawValueBody) referencedTypes() []string       { return nil }

func (e *enumeration) constantNames() []string {
	names := make([]string, len(e.Values))
	for i, value := range e.Values {
		names[i] = value.GoName
	}
	return names
}

// structure returns a struct definition's body. The planner checked the kind
// before handing the definition on, so any other kind here is its defect.
func (d *Def) structure() *structBody {
	body, isStruct := d.Body.(*structBody)
	if !isStruct {
		panic(fmt.Sprintf("schemagen: %s is a %T, not a struct", d.GoName, d.Body))
	}
	return body
}

func (d *Def) exported() bool { return token.IsExported(d.GoName) }

// A CatchAll is everything the open arm of an object union needs: which
// discriminant values are reserved, which properties the composed object
// declares, and the alternatives the arm is itself a union of.
type CatchAll struct {
	TagProperty  string
	ReservedTags []string
	// ExcludedExtraKeys is the complete declared-property set of the composed
	// object, including its common fields. Extra may own only the remainder.
	ExcludedExtraKeys []string
	Alternatives      []catchAllAlternative
}

// A catchAllAlternative is one arm of the union a catch-all arm may itself be.
type catchAllAlternative struct {
	GoName   string
	Required []string
}

// A Projection derives a handle-facing params type from a wire request: the
// request minus the identifiers its handle supplies.
type Projection struct {
	From *Def
	// Bound are the source fields the handle supplies, in the manifest's order.
	Bound []*Field
}

// An EnumValue is one constant arm of a string or number union.
type EnumValue struct {
	GoName string
	Value  string
	Doc    []string
}

// An Arm is one arm of a union.
type Arm struct {
	Tag string // "" when the arm has no discriminant constant
	// TagConst is the generated constant spelling Tag, which the codec and
	// hand-written code share.
	TagConst string
	GoType   string
	CatchAll bool
	// Untagged marks the arm that applies when the discriminant is absent: it
	// declares the discriminant as null rather than pinning it to a constant.
	Untagged bool
	// IsNull marks the null arm of a value union, which is selected before any
	// other because every other arm's decoder refuses null.
	IsNull bool
}

// armTypes are the Go types of a union's arms, in schema order.
func armTypes(arms []*Arm) []string {
	types := make([]string, len(arms))
	for i, arm := range arms {
		types[i] = arm.GoType
	}
	return types
}

// A Field is one property of a generated struct.
type Field struct {
	JSONName string
	GoName   string
	Doc      []string

	Required bool
	Nullable bool
	Opt      bool // the Go type is Opt[...], so absent and null stay apart

	Value         *ValueType
	Skip          bool // x-deserialize-skip-invalid-items
	DefaultOnNull bool
	Fallback      fallbackKind
	// Default is the schema's declared default, or nil when it declares none.
	Default *declaredDefault
}

// A declaredDefault is a property's schema default.
type declaredDefault struct {
	// Literal is the default as a Go literal in the property's value type.
	Literal string
	// Zero reports a default equal to its type's zero value, which a property
	// reset before decoding already holds.
	Zero bool
}

// A fallbackKind is what a property marked x-deserialize-default-on-error
// becomes when its value does not decode: the declared default if there is one,
// an empty array for an array that cannot be null, and otherwise nothing.
type fallbackKind int

const (
	fbNone fallbackKind = iota
	fbAbsent
	fbEmptySlice
	fbDefault
)

type valueKind int

const (
	vScalar valueKind = iota
	vRef
	// vBound is a definition whose grammar a hand-written Go type owns.
	vBound
	vSlice
	vMap
	vMeta
	// vAny is a property the schema constrains not at all.
	vAny
)

// A ValueType is a property's type with nullability stripped off, because
// nullability is a property of the property and not of the type it carries.
type ValueType struct {
	Kind valueKind
	Go   string // the Go type expression
	// Ident is the exported spelling of Go, which a union's selector is named
	// after.
	Ident   string
	Ref     string // schema definition name, for vRef
	IsUnion bool   // a vRef whose definition has a generated selector
	// Format is the schema's `format` for a string, enforced by the codec.
	Format    string
	MinLength uint64
	Elem      *ValueType
}

type planner struct {
	doc   *Document
	names *names

	internal map[string]bool
	bindings map[string]*Binding

	goNames map[string]string // schema definition name -> Go type name
	idents  map[string]string // schema definition name -> exported spelling
	shapes  map[string]defShape
	// armPayloads counts, over the whole schema, how many union arms use each
	// definition as their sole payload. A payload used by exactly one arm can be
	// that arm's Go type.
	armPayloads map[string]int
	arms        map[armKey]allocatedArm

	planned  map[string]*Def // schema definition name -> its type, once planned
	planning map[string]bool
	defs     map[string]*Def // Go name -> every generated type
}

type armKey struct {
	union string
	index int
}

type allocatedArm struct {
	goType string
	ident  string
}

func newPlanner(doc *Document) *planner {
	return &planner{
		doc:         doc,
		names:       newNames(),
		internal:    make(map[string]bool),
		bindings:    make(map[string]*Binding),
		goNames:     make(map[string]string),
		idents:      make(map[string]string),
		shapes:      make(map[string]defShape),
		armPayloads: make(map[string]int),
		arms:        make(map[armKey]allocatedArm),
		planned:     make(map[string]*Def),
		planning:    make(map[string]bool),
		defs:        make(map[string]*Def),
	}
}

// Plan resolves the manifest into everything the emitter needs.
func (p *planner) Plan(manifest *Manifest, schemaTag string) (*Plan, error) {
	for name := range manifest.Internal {
		p.internal[name] = true
	}
	maps.Copy(p.bindings, manifest.Bindings)
	if err := p.allocate(); err != nil {
		return nil, err
	}
	for _, name := range manifest.rootsWithoutProbes() {
		if _, err := p.plan(name); err != nil {
			return nil, fmt.Errorf("%s%s: %w", definitionPointer, name, err)
		}
	}
	for _, name := range sortedKeys(manifest.Probes) {
		if p.planned[name] != nil {
			return nil, fmt.Errorf("probe %s is reachable from the other roots; remove it from the manifest", name)
		}
		if _, err := p.plan(name); err != nil {
			return nil, fmt.Errorf("%s%s: %w", definitionPointer, name, err)
		}
	}
	for _, name := range sortedKeys(manifest.Projections) {
		if err := p.planProjection(name, manifest.Projections[name]); err != nil {
			return nil, fmt.Errorf("projection %s: %w", name, err)
		}
	}

	defs := slices.SortedFunc(maps.Values(p.defs), func(left, right *Def) int {
		return strings.Compare(left.GoName, right.GoName)
	})
	if err := checkVisibility(defs); err != nil {
		return nil, err
	}
	return &Plan{Defs: defs, Closure: sortedKeys(p.planned), SchemaTag: schemaTag}, nil
}

// add registers a generated type under its Go name.
func (p *planner) add(def *Def) {
	p.defs[def.GoName] = def
}

func (p *planner) plan(name string) (*Def, error) {
	if def, ok := p.planned[name]; ok {
		return def, nil
	}
	schema, defined := p.doc.Defs[name]
	if !defined {
		return nil, fmt.Errorf("%q is not a definition the schema has", name)
	}
	if p.bindings[name] != nil {
		return nil, fmt.Errorf("%s is bound to %s, so it is not generated", name, p.bindings[name].GoType)
	}
	if p.planning[name] {
		return nil, fmt.Errorf("%s refers to itself, and recursive definitions are not implemented", name)
	}
	p.planning[name] = true
	defer delete(p.planning, name)

	shape, err := p.shape(name)
	if err != nil {
		return nil, err
	}
	def := &Def{
		SchemaName:  name,
		GoName:      p.goNames[name],
		Ident:       p.idents[name],
		Description: schema.Description,
	}
	if planErr := p.planShape(def, schema, shape); planErr != nil {
		return nil, planErr
	}
	p.planned[name] = def
	p.add(def)
	return def, nil
}

func (p *planner) shape(name string) (defShape, error) {
	if shape, ok := p.shapes[name]; ok {
		return shape, nil
	}
	shape, err := classify(p.doc.Defs[name])
	if err != nil {
		return shapeUnknown, err
	}
	p.shapes[name] = shape
	return shape, nil
}

func (p *planner) planShape(def *Def, schema *Schema, shape defShape) error {
	switch shape {
	case shapeObjectUnion:
		return p.planObjectUnion(def, schema)
	case shapeStringEnum:
		def.Doc = docComment(def.GoName, schema.Description)
		return p.planStringUnion(def, schema.Arms())
	case shapeIntegerEnum:
		def.Doc = docComment(def.GoName, schema.Description)
		return p.planNumberUnion(def, schema.Arms())
	case shapeStruct:
		def.Doc = docComment(def.GoName, schema.Description)
		_, err := p.planStruct(def, schema, "")
		return err
	case shapeFlattenedUnion:
		def.Doc = docComment(def.GoName, schema.Description)
		owned, err := unionOwnedProperty(schema)
		if err != nil {
			return err
		}
		body, err := p.planStruct(def, schema, owned)
		if err != nil {
			return err
		}
		return p.planFlattenedUnion(def, body, schema)
	case shapeStringNewtype, shapeNumericNewtype:
		def.Doc = docComment(def.GoName, schema.Description)
		base, err := p.valueType(schema)
		if err != nil {
			return err
		}
		def.Body = &newtypeBody{Base: base}
		return nil
	case shapeValueUnion:
		return p.planValueUnion(def, schema.Arms())
	case shapeRaw:
		def.Doc = docComment(def.GoName, schema.Description)
		def.Body = rawValueBody{}
		return nil
	default:
		return fmt.Errorf("unimplemented definition shape: keywords %v", schema.Keywords)
	}
}

// checkVisibility refuses an exported type that names an unexported one, at any
// depth: an importer could hold the value but could not write the field's type,
// construct one, or switch on it.
func checkVisibility(defs []*Def) error {
	unexported := make(map[string]bool)
	for _, def := range defs {
		if !def.exported() {
			unexported[def.GoName] = true
		}
	}
	for _, def := range defs {
		if !def.exported() {
			continue
		}
		for _, name := range def.Body.referencedTypes() {
			if unexported[name] {
				return fmt.Errorf(
					"%s is exported and reaches the unexported %s; export that definition or make %s internal too",
					def.GoName, name, def.GoName)
			}
		}
	}
	return nil
}

// namedTypes returns every generated type a value type names, through its
// containers.
func (v *ValueType) namedTypes() []string {
	var named []string
	for value := v; value != nil; value = value.Elem {
		if value.Kind == vRef {
			named = append(named, value.Go)
		}
	}
	return named
}
