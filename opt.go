package acp

import (
	"encoding/json"
	"fmt"
)

// An Opt holds an optional JSON property that the schema also permits to be
// null, and keeps its three states apart: absent, null, and present with a value.
//
// The schema distinguishes an omitted property from one present as null, which a
// nil pointer cannot. The zero Opt is absent, and the omitzero tag option keeps
// it out of the message through [Opt.IsZero]. Where the schema calls the two
// equivalent, such as a capability that is not advertised, both states are still
// kept: what they mean is the domain's decision, not the codec's.
//
// A generated field is decoded by the schema-directed codec, which leaves an
// unreadable optional property absent rather than refusing the message around it.
// An Opt in a caller's own type has no schema behind it, so [Opt.UnmarshalJSON]
// reports the failure as encoding/json does.
type Opt[T any] struct {
	state optState
	value T
}

type optState uint8

const (
	optAbsent optState = iota
	optNull
	optPresent
)

func OptValue[T any](v T) Opt[T] {
	return Opt[T]{state: optPresent, value: v}
}

// OptNull is present as null, which is not the same as absent.
func OptNull[T any]() Opt[T] {
	return Opt[T]{state: optNull}
}

// Get reports false for both the absent and the null state. [Opt.IsZero] and
// [Opt.IsNull] tell those two apart.
func (o Opt[T]) Get() (T, bool) {
	return o.value, o.state == optPresent
}

// IsZero reports whether the property is absent. The encoder consults it through
// the omitzero tag option, which is what keeps an absent property out of the
// message entirely rather than sending it as null.
func (o Opt[T]) IsZero() bool {
	return o.state == optAbsent
}

func (o Opt[T]) IsNull() bool {
	return o.state == optNull
}

func (o Opt[T]) MarshalJSON() ([]byte, error) {
	if o.state != optPresent {
		return []byte("null"), nil
	}
	data, err := json.Marshal(o.value)
	if err != nil {
		return nil, fmt.Errorf("acp: marshalling optional value: %w", err)
	}
	return data, nil
}

func (o *Opt[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*o = OptNull[T]()
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*o = OptValue(v)
	return nil
}
