package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
)

// DecodeObject splits a JSON object into its properties without decoding their
// values.
//
// Per-property recovery needs this. A property marked
// x-deserialize-default-on-error must be able to fail on its own and be replaced
// by its default while the rest of the message decodes, and a decoder that
// unmarshalled the whole object in one pass could only fail all of it or none of
// it.
func DecodeObject(data []byte) (map[string]json.RawMessage, error) {
	if kind(data) != '{' {
		return nil, fmt.Errorf("expected a JSON object, got %s", describe(data))
	}
	obj := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// RawItems splits a JSON array into its elements without decoding them, which
// is what lets an array marked x-deserialize-skip-invalid-items drop the
// elements that fail and keep the rest.
func RawItems(data []byte) ([]json.RawMessage, error) {
	if kind(data) != '[' {
		return nil, fmt.Errorf("expected a JSON array, got %s", describe(data))
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// IsNull reports whether data is the JSON null literal. A property the schema
// permits to be null needs this before decoding, because null decodes into a Go
// map, slice or pointer without complaint and would arrive as the absent state.
func IsNull(data []byte) bool {
	return kind(data) == 'n'
}

// Extra returns the properties of obj that declared does not name, which is a
// catch-all union arm's payload: the schema gives such an arm
// additionalProperties, so the keys its own subschemas do not evaluate are the
// vendor's and have to survive.
func Extra(obj map[string]json.RawMessage, declared ...string) map[string]json.RawMessage {
	extra := make(map[string]json.RawMessage, len(obj))
	for name, raw := range obj {
		if slices.Contains(declared, name) {
			continue
		}
		extra[name] = raw
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// An Optional is a value that knows whether it is absent, which is how the
// writer omits an absent property instead of encoding it as null. The exported
// Opt type satisfies it.
type Optional interface {
	IsZero() bool
	MarshalJSON() ([]byte, error)
}

// An ObjectWriter builds a JSON object one property at a time, in the order the
// properties are set.
//
// Generated encoders set them in the order the schema declares them, so a
// message can be read against the schema that defines it, and so golden output
// does not move when an unrelated field is added. It also gives a union's
// discriminant a defined position — first — which json.Marshal over a map
// cannot.
//
// A failure is kept and returned from [ObjectWriter.Bytes] rather than reported
// per call, because a generated encoder that had to check every property would
// be several times its size for a failure mode that means a Go value cannot be
// encoded at all.
type ObjectWriter struct {
	buf []byte
	err error
}

// Set encodes v as the value of property name.
func (w *ObjectWriter) Set(name string, v any) {
	if w.err != nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		w.err = At(name, err)
		return
	}
	w.SetRaw(name, raw)
}

// SetRaw writes already-encoded JSON as the value of property name. The caller
// owns raw's validity: generated code passes the output of another encoder, and
// checking it again would parse every message twice.
func (w *ObjectWriter) SetRaw(name string, raw []byte) {
	if w.err != nil {
		return
	}
	key, err := json.Marshal(name)
	if err != nil {
		w.err = At(name, err)
		return
	}
	if len(w.buf) == 0 {
		w.buf = append(w.buf, '{')
	} else {
		w.buf = append(w.buf, ',')
	}
	w.buf = append(w.buf, key...)
	w.buf = append(w.buf, ':')
	w.buf = append(w.buf, raw...)
}

// SetOptional writes property name unless v is absent, in which case it writes
// nothing at all. Present-as-null is written as null: the schema distinguishes
// the two states and so does the encoder.
func (w *ObjectWriter) SetOptional(name string, v Optional) {
	if w.err != nil || v.IsZero() {
		return
	}
	raw, err := v.MarshalJSON()
	if err != nil {
		w.err = At(name, err)
		return
	}
	w.SetRaw(name, raw)
}

// Embed writes another object's properties into this one, without the object.
//
// The schema flattens a union into an object in a handful of places: the type has
// properties of its own and one of several kind-specific shapes, all in the same
// JSON object. Encoding the two halves separately and splicing them is what keeps
// the union a union in Go while the wire stays flat.
func (w *ObjectWriter) Embed(raw []byte) {
	if w.err != nil {
		return
	}
	if kind(raw) != '{' {
		w.err = fmt.Errorf("an embedded value must be an object, got %s", describe(raw))
		return
	}
	// json.Marshal emits no insignificant whitespace, so the byte after '{' is
	// either '}' for an empty object or the first key's opening quote. An empty
	// one contributes nothing.
	body := raw[1 : len(raw)-1]
	if kind(body) == 0 {
		return
	}
	if len(w.buf) == 0 {
		w.buf = append(w.buf, '{')
	} else {
		w.buf = append(w.buf, ',')
	}
	w.buf = append(w.buf, body...)
}

// SetExtra writes the retained properties of a catch-all union arm, in sorted
// order so that one Go value always encodes the same way. Unlike
// [ObjectWriter.SetRaw] it validates, because these values came from a peer or
// from a caller rather than from a generated encoder.
func (w *ObjectWriter) SetExtra(extra map[string]json.RawMessage) {
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		if w.err != nil {
			return
		}
		raw := extra[name]
		if !json.Valid(raw) {
			w.err = At(name, errRetainedNotJSON)
			return
		}
		w.SetRaw(name, raw)
	}
}

// Bytes returns the object, or the first failure any property encoding hit.
func (w *ObjectWriter) Bytes() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if len(w.buf) == 0 {
		return []byte("{}"), nil
	}
	return append(w.buf, '}'), nil
}

// errRetainedNotJSON reports a retained property whose bytes are not a JSON
// value. One of them would make the whole message unparseable at the far end.
var errRetainedNotJSON = errors.New("retained property is not valid JSON")

// kind returns the first significant byte of a JSON value, which is enough to
// tell the six JSON types apart, or 0 when there is none.
func kind(data []byte) byte {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

func describe(data []byte) string {
	switch k := kind(data); k {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	case 0:
		return "nothing"
	default:
		if k == '-' || (k >= '0' && k <= '9') {
			return "a number"
		}
		return "invalid JSON"
	}
}

// SatisfiesOneAlternative reports whether the retained properties decode as one of
// the alternatives a catch-all arm is a union of.
//
// A catch-all arm is not everything that is not a known arm. It has a schema of
// its own, and when that schema is a union — the elicitation modes each carry a
// scope union, and the custom mode carries it too — a value belongs to the arm
// only if it satisfies one alternative. Satisfying one means decoding as it: a
// numeric sessionId names the property the session scope requires and is still not
// a session scope, so the check runs each candidate's own codec rather than
// looking for its property names.
//
// No alternatives means the arm is not a union and every value satisfies it.
func SatisfiesOneAlternative(
	extra map[string]json.RawMessage,
	alternatives ...func([]byte) error,
) bool {
	if len(alternatives) == 0 {
		return true
	}
	encoded, err := json.Marshal(extra)
	if err != nil {
		return false
	}
	for _, decode := range alternatives {
		if decode(encoded) == nil {
			return true
		}
	}
	return false
}

// ValidateURI enforces `format: "uri"`, which the schema states once — on the URL
// a client sends a user to.
//
// A scheme is what separates a URI from a string that looks like one: net/url
// parses "not a url" and "/sign-in" without complaint, and both would send a user
// nowhere. Requiring one matches the reference implementation on every value the
// fixture corpus asks it about, including the two that are URIs without being web
// addresses — mailto: and urn:.
// ValidateURI enforces `format: "uri"`, which the schema states once — on the URL
// a client sends a user to.
//
// A scheme is what separates a URI from a string that looks like one: net/url
// parses "not a url" and "/sign-in" without complaint, and both would send a user
// nowhere. Requiring one matches the reference implementation on every value the
// fixture corpus asks it about, including the two that are URIs without being web
// addresses — mailto: and urn:.
func ValidateURI(property, value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return At(property, fmt.Errorf("acp: %q is not a URI: %w", value, err))
	}
	if !parsed.IsAbs() {
		return At(property, fmt.Errorf("acp: %q is not a URI: it names no scheme", value))
	}
	return nil
}

// UnmarshalURI is the reading half of [ValidateURI]: a value this side would
// refuse to send is one it refuses to accept.
func UnmarshalURI(property string, data []byte) (string, error) {
	value, err := UnmarshalValue[string](data)
	if err != nil {
		return "", err
	}
	return value, ValidateURI(property, value)
}

// MarshalURI is the writing half.
func MarshalURI(property, value string) ([]byte, error) {
	if err := ValidateURI(property, value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
