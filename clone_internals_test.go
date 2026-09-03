package acp

import (
	"reflect"
	"testing"
)

// The boundary clone.go depends on: everything deepCopy is applied to can be
// copied. A union arm may carry unexported state and one deliberately does — the
// elicitation scope naming a JSON-RPC request identifier — and copying that arm
// panics, so what keeps this true is that nothing retained holds one.
//
// It walks types rather than values because the failure is a type appearing in the
// graph, not a value taking a path. It stops at deepCopier, which is where
// reflection stops needing to reach, and at interfaces, whose arms cannot be listed
// from the type; those are covered by the value-based tests in clone_test.go.
//
// The roots are the copied values, not the configurations holding them: a
// configuration is copied field by field on purpose, because its Logger is meant to
// be shared and its handlers are functions whose identity is the point.
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

// The modes are roots because the operation copies one before writing the scope
// into it, and their Value is an interface the walk stops at — so what keeps the
// unexported arm out there is checkedMode, not the type graph
// (TestASuppliedScopeIsRefused is that half). This is the other: the copier really
// does refuse, which is what makes the walk worth running.
func TestDeepCopyRefusesUnexportedState(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("deep-copying a value with unreachable state was allowed, so it would have " +
				"been shared behind the caller's back")
		}
	}()
	_ = deepCopy(CreateElicitationRequestValue(&ElicitationFormMode{
		Value: &ElicitationFormModeRequest{},
	}))
}

func findUnexported(typ reflect.Type, seen map[reflect.Type]bool, path string) string {
	if typ == nil || seen[typ] {
		return ""
	}
	seen[typ] = true

	// Both spellings: deepCopySelf may be declared on the value or on the pointer.
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
		// An interface cannot name its arms; scalars and funcs have nothing to reach.
		return ""
	}
}
