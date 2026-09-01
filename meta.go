package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Meta owns the protocol's reserved _meta object.
//
// Keeping encoded JSON values is intentional: the protocol promises unknown JSON,
// not arbitrary Go object identity. This makes a Meta value fully copyable and
// prevents an object with hidden mutable state from escaping through an otherwise
// immutable configuration or peer snapshot.
type Meta struct { //nolint:recvcheck // encoding/json requires MarshalJSON on values and UnmarshalJSON on pointers.
	values map[string]json.RawMessage
}

// NewMeta encodes immediately so later mutations of the source values cannot
// change a configuration or peer snapshot behind the library's back.
func NewMeta(values map[string]any) (Meta, error) {
	var meta Meta
	for key, value := range values {
		if err := meta.Set(key, value); err != nil {
			return Meta{}, err
		}
	}
	return meta, nil
}

// Set encodes before retaining a value, so an unsupported Go value fails at the
// boundary instead of making an unrelated future protocol write fail.
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

// Decode keeps the encoded representation private while preserving the
// distinction between an absent key and a present JSON null.
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

// Delete changes the owned object without exposing its backing map.
func (m *Meta) Delete(key string) {
	if m != nil {
		delete(m.values, key)
	}
}

// Len lets constructors omit an empty optional _meta without exposing storage.
func (m Meta) Len() int { return len(m.values) }

// MarshalJSON keeps the zero Meta an object rather than JSON null.
func (m Meta) MarshalJSON() ([]byte, error) {
	if m.values == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m.values)
}

// UnmarshalJSON rejects null because absence and explicit null belong to Opt,
// while a present Meta must be an object.
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
