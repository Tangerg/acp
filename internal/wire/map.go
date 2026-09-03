package wire

import (
	"encoding/json"
	"slices"
)

// The map counterpart of slice.go, for the one thing a JSON object keyed by
// arbitrary names has in common with an array: its values may be a union.
//
// A union's discriminant is written by the union and not by the arm — TextContent
// encodes as {"text":"hi"} and marshalContentBlock adds {"type":"text"} around it
// — and a Go interface cannot decode into itself. A map of unions handed to
// encoding/json therefore loses the discriminant going out and cannot be read at
// all coming in: a message this side put on the wire and could not take back.

// UnmarshalMapFunc decodes into a map whose values a generated function selects
// the arm of. Keys are visited in sorted order so that a document with two
// undecodable values reports the same one every run.
func UnmarshalMapFunc[T any](
	data []byte,
	decode func(json.RawMessage) (T, error),
) (map[string]T, error) {
	object, err := DecodeObject(data)
	if err != nil {
		return nil, err
	}
	out := make(map[string]T, len(object))
	for _, key := range slices.Sorted(mapKeys(object)) {
		value, err := decode(object[key])
		if err != nil {
			return nil, At(key, err)
		}
		out[key] = value
	}
	return out, nil
}

// MarshalMapFunc encodes a nil map as {} rather than null, for the reason
// [MarshalSlice] gives about arrays: the schema requires an object wherever this
// is reached, and a caller who left a required map alone should not send null.
//
// Keys are sorted. A peer must not care about object order, but a golden test, a
// transcript diff and a person reading a log all do.
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

func mapKeys[T any](m map[string]T) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}
