package wire

import (
	"encoding/json"
	"fmt"
)

// Tag reads a union's discriminant, and reports absent for a property that is
// present but not a string.
//
// That is the reference implementation's rule, and it decides which arm a value
// lands in: a non-string discriminant does not select the arm whose constant it
// might have matched, it falls through to the arms that do not have one.
func Tag(obj map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := obj[name]
	if !ok {
		return "", false
	}
	// UnmarshalValue rather than json.Unmarshal, because json.Unmarshal accepts
	// null into a string: it leaves the string alone and reports nothing, so a
	// null discriminant would arrive here as the empty-string tag and select an
	// arm — or fail the union — where the rule is that it selects neither.
	tag, err := UnmarshalValue[string](raw)
	if err != nil {
		return "", false
	}
	return tag, true
}

// TagObject encodes v and inserts the union's discriminant as its first
// property.
//
// The discriminant belongs to the union rather than to the arm because the
// schema puts it there: ContentChunk is the payload of three different
// SessionUpdate arms, and ToolCallUpdate is both a SessionUpdate arm and an
// ordinary property of RequestPermissionRequest. A payload type that wrote its
// own tag could serve neither case. The generator refuses to emit an arm whose
// payload declares a property with the discriminant's name, which is what makes
// splicing safe without re-parsing v's output to check for a duplicate key.
func TagObject(name, tag string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if kind(raw) != '{' {
		return nil, fmt.Errorf("a union arm must encode as an object, got %s", describe(raw))
	}
	key, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	value, err := json.Marshal(tag)
	if err != nil {
		return nil, err
	}

	// json.Marshal emits no insignificant whitespace, so the byte after '{' is
	// either '}' for an empty object or the first key's opening quote.
	rest := raw[1:]
	out := make([]byte, 0, len(raw)+len(key)+len(value)+2)
	out = append(out, '{')
	out = append(out, key...)
	out = append(out, ':')
	out = append(out, value...)
	if kind(rest) != '}' {
		out = append(out, ',')
	}
	return append(out, rest...), nil
}
