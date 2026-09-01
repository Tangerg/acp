package acp

import (
	"encoding/json"
	"fmt"
)

// An Opt holds an optional JSON property that the schema also permits to be
// null, and keeps the three states those two facts produce apart: absent, null,
// and present with a value.
//
// The stable schema distinguishes an omitted property from one present as null in 222
// places, so a pointer with omitempty cannot carry this part of the grammar — a
// nil pointer would have to mean both. The zero Opt is the absent state, which
// is what the encoder's omitzero tag option consults through [Opt.IsZero]: an
// absent field emits nothing while an explicit null emits null.
//
// Where the schema says the two are semantically equivalent — several capability
// fields document that omitted and null both mean not advertised — the wire
// representation still retains both states. Domain code decides whether they
// mean the same thing; the codec does not erase a distinction the schema permits.
//
// An absent Opt asked for JSON anyway encodes as null, because there is nothing
// else honest to answer: omitting it is the encoder's job, not this type's.
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
