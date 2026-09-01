package wire

import "encoding/json"

// UnmarshalValue decodes one value, and refuses null.
//
// The refusal is the point. Generated code calls this for a property the schema
// does not permit to be null, and json.Unmarshal would accept null there
// silently: it leaves the Go value untouched and reports no error, so the
// property would arrive as its zero value. A nullable property is decoded
// through Opt instead, which has a state for null.
func UnmarshalValue[T any](data []byte) (T, error) {
	var v T
	if IsNull(data) {
		return v, ErrNotNullable
	}
	err := json.Unmarshal(data, &v)
	return v, err
}

// UnmarshalSlice decodes a JSON array, failing on the first element that does
// not decode.
func UnmarshalSlice[T any](data []byte) ([]T, error) {
	return UnmarshalSliceFunc(data, decodeValue[T])
}

// UnmarshalSliceSkippingInvalid decodes a JSON array, dropping the elements that
// do not decode.
//
// This implements x-deserialize-skip-invalid-items, which 35 arrays in the
// schema carry: one unrecognised item in a list of MCP servers or content roles
// must not cost the message. The array itself still has to be an array — the
// keyword salvages elements, not the value they are in.
func UnmarshalSliceSkippingInvalid[T any](data []byte) ([]T, error) {
	return UnmarshalSliceFuncSkippingInvalid(data, decodeValue[T])
}

// UnmarshalSliceFunc is [UnmarshalSlice] for an element type whose decoding is
// not json.Unmarshal — a union, which is selected by a generated function
// because a Go interface cannot decode into itself.
func UnmarshalSliceFunc[T any](data []byte, decode func(json.RawMessage) (T, error)) ([]T, error) {
	items, err := RawItems(data)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for i, item := range items {
		v, err := decode(item)
		if err != nil {
			return nil, Index(i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// UnmarshalSliceFuncSkippingInvalid is [UnmarshalSliceSkippingInvalid] for an
// element type whose decoding is not json.Unmarshal.
func UnmarshalSliceFuncSkippingInvalid[T any](data []byte, decode func(json.RawMessage) (T, error)) ([]T, error) {
	items, err := RawItems(data)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		v, err := decode(item)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// MarshalSlice encodes a slice, and encodes a nil one as an empty array.
//
// A nil Go slice marshals as null, which is invalid wherever the schema
// requires an array — and "required" is exactly where a caller is most likely to
// have left the field alone.
func MarshalSlice[T any](values []T) ([]byte, error) {
	return MarshalSliceFunc(values, encodeValue[T])
}

// MarshalSliceFunc is [MarshalSlice] for an element type whose encoding is not
// json.Marshal — a union, whose discriminant is written by the union rather
// than by the arm.
func MarshalSliceFunc[T any](values []T, encode func(T) ([]byte, error)) ([]byte, error) {
	out := make([]byte, 0, 2+len(values)*32)
	out = append(out, '[')
	for i, v := range values {
		if i > 0 {
			out = append(out, ',')
		}
		raw, err := encode(v)
		if err != nil {
			return nil, Index(i, err)
		}
		out = append(out, raw...)
	}
	return append(out, ']'), nil
}

func decodeValue[T any](data json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}

func encodeValue[T any](v T) ([]byte, error) {
	return json.Marshal(v)
}
