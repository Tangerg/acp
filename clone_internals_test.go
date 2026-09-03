package acp

import (
	"reflect"
	"testing"
)

// The boundary clone.go depends on: everything reachable from a value this
// package retains and hands back can be deep-copied.
//
// clone.go used to claim a test held this and none did, which is how the claim
// survived becoming false. It became false when elicitation was generated: a
// union arm may carry unexported state, and one of them deliberately does — the
// scope naming a JSON-RPC request identifier, which this API does not hand out.
// Copying that arm panics. Nothing retained holds one, and this is what says so
// rather than hoping.
//
// It walks types rather than a value because the failure it guards is a type
// appearing in the graph, not a particular value taking a path through it. A
// configuration that came to hold such a type would panic on the copy, at
// construction, in a library.
//
// The walk stops at two places, both deliberate. A type implementing deepCopier
// owns its representation, which is where reflection stops needing to reach. An
// interface's arms cannot be listed from its type, so those are covered by the
// value-based tests in clone_test.go, which copy real configurations and real
// snapshots.
//
// The roots are what deepCopy is actually applied to, not the configurations that
// contain them. A configuration is copied field by field on purpose: its Logger is
// shared because a logger is meant to be, and its handlers are functions, so
// copying the whole struct would be copying things whose identity is the point.
func TestEverythingRetainedCanBeDeepCopied(t *testing.T) {
	for name, root := range map[string]reflect.Type{
		"Implementation":      reflect.TypeFor[Implementation](),
		"Meta":                reflect.TypeFor[Meta](),
		"ClientCapabilities":  reflect.TypeFor[ClientCapabilities](),
		"AgentCapabilities":   reflect.TypeFor[AgentCapabilities](),
		"PeerInfo":            reflect.TypeFor[PeerInfo](),
		"AuthMethodAgent":     reflect.TypeFor[AuthMethodAgent](),
		"AuthMethodTerminal":  reflect.TypeFor[AuthMethodTerminal](),
		"ElicitationFormMode": reflect.TypeFor[ElicitationFormMode](),
		"ElicitationURLMode":  reflect.TypeFor[ElicitationURLMode](),
	} {
		t.Run(name, func(t *testing.T) {
			if path := findUnexported(root, map[reflect.Type]bool{}, name); path != "" {
				t.Fatalf("%s is reachable from a retained value and carries unexported state, "+
					"so deep-copying one would panic where clone.go says it cannot", path)
			}
		})
	}
}

// The two elicitation modes are roots because the operation copies one before
// writing the scope into it. Their Value is an interface, so the walk stops there
// — and the arm behind it is the one arm in the package that carries unexported
// state. What keeps it out is not the type graph but checkedMode, which refuses a
// mode that already has a scope; TestASuppliedScopeIsRefused is that half.
//
// This is the other half: a value that does carry unreachable state is refused by
// the copier rather than silently shared, which is what makes the walk above worth
// running at all.
func TestDeepCopyRefusesUnexportedState(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("deep-copying a value with unreachable state was allowed, so it would have " +
				"been shared behind the caller's back")
		}
	}()
	// The elicitation request scope, which is exactly the shape the walk exists to
	// keep out of anything retained.
	_ = deepCopy(CreateElicitationRequestValue(&ElicitationFormMode{
		Value: &ElicitationFormModeRequest{},
	}))
}

// findUnexported returns the path to a struct field reflection cannot write, or
// the empty string.
func findUnexported(typ reflect.Type, seen map[reflect.Type]bool, path string) string {
	if typ == nil || seen[typ] {
		return ""
	}
	seen[typ] = true

	// Both spellings, because deepCopySelf may be declared on the value or on the
	// pointer, and reaching a type either way means the same thing here.
	copier := reflect.TypeFor[deepCopier]()
	if typ.Implements(copier) || reflect.PointerTo(typ).Implements(copier) {
		return ""
	}

	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return findUnexported(typ.Elem(), seen, path+"[]")

	case reflect.Map:
		if found := findUnexported(typ.Key(), seen, path+"{key}"); found != "" {
			return found
		}
		return findUnexported(typ.Elem(), seen, path+"{}")

	case reflect.Struct:
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				return path + "." + field.Name
			}
			if found := findUnexported(field.Type, seen, path+"."+field.Name); found != "" {
				return found
			}
		}
		return ""

	default:
		// Scalars have nothing to reach, a func is returned as it is, and an
		// interface cannot name its arms.
		return ""
	}
}
