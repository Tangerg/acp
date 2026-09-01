package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Meta owns the protocol's reserved _meta object, the one place either peer may
// attach data the specification says nothing about.
//
// It keeps values encoded rather than as Go objects. What the protocol promises
// is unknown JSON, not Go object identity, and holding the encoded form is what
// makes a Meta fully copyable: an object with hidden mutable state cannot escape
// through a configuration or a peer snapshot that both claim to be immutable, and
// a value that cannot cross JSON fails at [Meta.Set] rather than at some later
// protocol write with no way to say which key was at fault.
//
// A present Meta must be a JSON object. Absence and an explicit null belong to
// [Opt], so decoding null into one is an error rather than an empty object.
type Meta struct { //nolint:recvcheck // encoding/json requires MarshalJSON on values and UnmarshalJSON on pointers.
	values map[string]json.RawMessage
}

// NewMeta encodes every value immediately, so that later mutations of the source
// cannot change a configuration or a snapshot behind this package's back.
func NewMeta(values map[string]any) (Meta, error) {
	var meta Meta
	for key, value := range values {
		if err := meta.Set(key, value); err != nil {
			return Meta{}, err
		}
	}
	return meta, nil
}

// Set encodes before retaining, so an unsupported Go value is refused here rather
// than making an unrelated protocol write fail later.
func (m *Meta) Set(key string, value any) error {
	if m == nil {
		return fmt.Errorf("acp: cannot set _meta key %q on a nil Meta", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("acp: encode _meta key %q: %w", key, err)
	}
	if m.values == nil {
		m.values = make(map[string]json.RawMessage)
	}
	m.values[key] = bytes.Clone(encoded)
	return nil
}

// Decode reports whether the key was present, which is how an absent key stays
// distinguishable from one present as JSON null.
func (m Meta) Decode(key string, target any) (bool, error) {
	encoded, ok := m.values[key]
	if !ok || target == nil {
		return ok, nil
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return true, fmt.Errorf("acp: decode _meta key %q: %w", key, err)
	}
	return true, nil
}

func (m *Meta) Delete(key string) {
	if m != nil {
		delete(m.values, key)
	}
}

func (m Meta) Len() int { return len(m.values) }

func (m Meta) MarshalJSON() ([]byte, error) {
	if m.values == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m.values)
}

func (m *Meta) UnmarshalJSON(data []byte) error {
	if m == nil {
		return errors.New("acp: decode _meta into nil Meta")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if values == nil {
		return errors.New("acp: _meta must be an object")
	}
	m.values = values
	return nil
}

func (m Meta) deepCopySelf() any {
	if m.values == nil {
		return Meta{}
	}
	values := make(map[string]json.RawMessage, len(m.values))
	for key, value := range m.values {
		values[key] = bytes.Clone(value)
	}
	return Meta{values: values}
}
