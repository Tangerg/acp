package wire

import (
	"encoding/json"
	"slices"
)

// The map counterpart of slice.go, for the one thing a JSON object keyed by
// arbitrary names has in common with an array: its values may be a union, and a
// union is not something encoding/json can carry on its own.
//
// A union's discriminant is written by the union and not by the arm — TextContent
// encodes as {"text":"hi"} and it is marshalContentBlock that adds
// {"type":"text"} around it — and a Go interface cannot decode into itself. So a
// map of unions handed to encoding/json loses the discriminant on the way out and
// cannot be read at all on the way in, which is a message this side put on the
// wire and could not take back.

// UnmarshalMapFunc decodes a JSON object into a map whose values decode through a
// function rather than through json.Unmarshal.
//
// Keys are the object's own, unescaped by the JSON decoder, so a property name
// with a slash or a tilde in it reports its failure through [At] like any other.
func UnmarshalMapFunc[T any](
	data []byte,
	decode func(json.RawMessage) (T, error),
) (map[string]T, error) {
	object, err := DecodeObject(data)
	if err != nil {
		return nil, err
	}
	out := make(map[string]T, len(object))
	// Sorted so that a document with two undecodable values reports the same one
	// every run. Go map iteration would otherwise make the failure depend on the
	// hash seed, which is the kind of test that passes until it does not.
	for _, key := range slices.Sorted(mapKeys(object)) {
		value, err := decode(object[key])
		if err != nil {
			return nil, At(key, err)
		}
		out[key] = value
	}
	return out, nil
}

// MarshalMapFunc encodes a map whose values encode through a function rather than
// through json.Marshal, and encodes a nil map as an empty object.
//
// Empty rather than null for the reason [MarshalSlice] gives about arrays: the
// schema requires an object wherever this is reached, and a caller who left a
// required map alone should not produce a message that says null.
//
// Keys are written in sorted order. JSON objects are unordered and a peer must
// not care, but a golden test, a diff of two transcripts and a person reading a
// log all care, and there is nothing to trade for it here.
func MarshalMapFunc[T any](values map[string]T, encode func(T) ([]byte, error)) ([]byte, error) {
	out := make([]byte, 0, 2+len(values)*32)
	out = append(out, '{')
	for i, key := range slices.Sorted(mapKeys(values)) {
		if i > 0 {
			out = append(out, ',')
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, At(key, err)
		}
		out = append(out, name...)
		out = append(out, ':')

		raw, err := encode(values[key])
		if err != nil {
			return nil, At(key, err)
		}
		out = append(out, raw...)
	}
	return append(out, '}'), nil
}

// mapKeys avoids importing maps for one function that returns an iterator over
// keys, which slices.Sorted then consumes.
func mapKeys[T any](m map[string]T) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}
