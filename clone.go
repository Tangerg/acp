package acp

import "reflect"

// One deep-copy boundary, for the values this package retains after construction
// and hands back out.
//
// Two contracts need it. A configuration is validated once and read for the life
// of a client or agent, so a caller who could still reach inside it could change
// what is advertised without going back through the check that made it valid —
// and could race the encoder writing it. And [ClientConn.Peer] calls itself an
// immutable snapshot, which is a claim about the whole value rather than about
// its outermost struct.
//
// It is one reflective function rather than a clone method per type because the
// types are generated from the schema and the tree is deep: every capability
// group carries a reserved _meta map, and there are more than twenty of them
// nested inside each other. Twenty hand-written copies would be a second
// description of a shape the schema already owns, and a schema bump would make
// them quietly wrong rather than loudly broken.
//
// What it copies is what JSON has: maps, slices, pointers, and interfaces, all
// the way down. Everything else is copied by assignment, which is what a struct
// copy already does correctly. It preserves the dynamic types it finds, which a
// round trip through the codec would not: a caller who put an int in _meta gets
// an int back.

// deepCopy returns a copy of value that shares no mutable memory with it.
func deepCopy[T any](value T) T {
	var copied T
	reflect.ValueOf(&copied).Elem().Set(copyValue(reflect.ValueOf(&value).Elem()))
	return copied
}

// A deepCopier copies itself, for types whose contents reflection cannot reach.
//
// [Opt] is the only one. Its three states live in unexported fields, which
// reflection may read and may not write, so the copy has to be made from inside
// the type. Every generated struct's fields are exported, so nothing else needs
// this — and a test holds that fact against the generated types.
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
			field := copied.Field(i)
			if !field.CanSet() {
				// Unexported, so nothing here can reach it either. A type that
				// hides mutable state implements deepCopier; see the test that
				// holds every type in this boundary to one or the other.
				continue
			}
			field.Set(copyValue(value.Field(i)))
		}
		return copied

	default:
		// Scalars, funcs, channels: assignment is the copy, and anything a caller
		// put in a _meta map that is not JSON-shaped stays shared because this
		// package has no idea what copying it would mean.
		return value
	}
}
