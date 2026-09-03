package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Meta owns the protocol's reserved _meta object for extension data.
//
// Values are encoded when set. This makes copies independent of mutable source
// objects and reports unsupported values at [Meta.Set], where the key is known.
//
// A present Meta must be a JSON object. Absence and an explicit null belong to
// [Opt], so decoding null into one is an error rather than an empty object.
type Meta struct { //nolint:recvcheck // encoding/json requires MarshalJSON on values and UnmarshalJSON on pointers.
	values map[string]json.RawMessage
}

// NewMeta constructs Meta from values that must all be JSON-encodable.
func NewMeta(values map[string]any) (Meta, error) {
	var meta Meta
	for key, value := range values {
		if err := meta.Set(key, value); err != nil {
			return Meta{}, err
		}
	}
	return meta, nil
}

// Set encodes and stores value under key. Later mutations of value do not affect
// the stored JSON.
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

// Delete is safe on a nil Meta, so a caller need not know whether one was set.
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
