package acp

import "reflect"

// One deep-copy boundary, for the values this package retains and hands back
// out: a validated configuration, which a caller must not change behind the
// check that made it valid or race the encoder writing it, and the [Negotiated]
// snapshot a connection reports.
//
// It is one reflective function rather than a clone method per type because the
// retained types are generated and deeply nested. Hand-written copies would be a
// second description of a shape the schema owns, and a schema bump would make
// them quietly wrong rather than loudly broken.
//
// Reaching unexported state is an invariant violation, not a value to share:
// jsonrpc.ID inside an elicitation scope has some, and nothing retained holds
// one. clone_internals_test.go walks the retained types to hold that boundary.
func deepCopy[T any](value T) T {
	var copied T
	reflect.ValueOf(&copied).Elem().Set(copyValue(reflect.ValueOf(&value).Elem()))
	return copied
}

// A deepCopier copies itself, because reflection may read unexported fields but
// not write them. [Opt] and [Meta] are the two retained types that need it.
type deepCopier interface {
	deepCopySelf() any
}

func (o Opt[T]) deepCopySelf() any {
	if o.state != optPresent {
		// Absent and null carry nothing to copy.
		return o
	}
	return Opt[T]{state: o.state, value: deepCopy(o.value)}
}

func copyValue(value reflect.Value) reflect.Value {
	if value.CanInterface() {
		if copier, ok := reflect.TypeAssert[deepCopier](value); ok {
			return reflect.ValueOf(copier.deepCopySelf())
		}
	}

	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			// A nil map is not an empty one on the wire, so it stays nil.
			return value
		}
		copied := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			copied.SetMapIndex(copyValue(iterator.Key()), copyValue(iterator.Value()))
		}
		return copied

	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		copied := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			copied.Index(i).Set(copyValue(value.Index(i)))
		}
		return copied

	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		copied := reflect.New(value.Type().Elem())
		copied.Elem().Set(copyValue(value.Elem()))
		return copied

	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		// Copy what it holds and rewrap it, so that the dynamic type survives.
		copied := reflect.New(value.Type()).Elem()
		copied.Set(copyValue(value.Elem()))
		return copied

	case reflect.Struct:
		copied := reflect.New(value.Type()).Elem()
		for i := range value.NumField() {
			if !copied.Field(i).CanSet() {
				panic("acp: deep copy reached unexported state in " + value.Type().String())
			}
			copied.Field(i).Set(copyValue(value.Field(i)))
		}
		return copied

	default:
		// Scalars and functions have no mutable representation to rebuild here.
		return value
	}
}
