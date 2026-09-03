package wire_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// A stand-in for a generated union: an interface whose arms are selected by a
// function, because that is the only thing the map helpers exist for.
type shape interface{ area() int }

type square struct {
	Side int `json:"side"`
}

func (s *square) area() int { return s.Side * s.Side }

// decodeShape is what a generated unmarshalX does: read the discriminant, then
// decode the arm it names.
func decodeShape(data json.RawMessage) (shape, error) {
	var tagged struct {
		Kind string `json:"kind"`
		Side int    `json:"side"`
	}
	if err := json.Unmarshal(data, &tagged); err != nil {
		return nil, err
	}
	if tagged.Kind != "square" {
		return nil, errors.New("not a shape this test knows")
	}
	return &square{Side: tagged.Side}, nil
}

// encodeShape is what a generated marshalX does: write the arm together with the
// discriminant the union owns, which the arm's own encoding does not carry.
func encodeShape(value shape) ([]byte, error) {
	s, ok := value.(*square)
	if !ok {
		return nil, errors.New("not a shape this test can write")
	}
	return json.Marshal(struct {
		Kind string `json:"kind"`
		Side int    `json:"side"`
	}{"square", s.Side})
}

// The round trip is the whole point: a map of unions written by the union's own
// encoder can be read back by its own decoder. Handed to encoding/json instead,
// the discriminant is not written and the value cannot be decoded at all.
func TestMapOfUnionsRoundTrips(t *testing.T) {
	values := map[string]shape{"a": &square{Side: 2}, "b": &square{Side: 3}}

	encoded, err := wire.MarshalMapFunc(values, encodeShape)
	if err != nil {
		t.Fatalf("MarshalMapFunc: %v", err)
	}
	const want = `{"a":{"kind":"square","side":2},"b":{"kind":"square","side":3}}`
	if string(encoded) != want {
		t.Fatalf("encoded %s, want %s", encoded, want)
	}

	decoded, err := wire.UnmarshalMapFunc(encoded, decodeShape)
	if err != nil {
		t.Fatalf("UnmarshalMapFunc: %v", err)
	}
	if len(decoded) != 2 || decoded["a"].area() != 4 || decoded["b"].area() != 9 {
		t.Fatalf("decoded %v, want the two squares back", decoded)
	}
}

// A nil map encodes as an empty object rather than as null, for the reason a nil
// slice encodes as []: the schema requires an object wherever this is reached, and
// a caller who left a required map alone should not produce a message that says
// null.
func TestANilMapEncodesAsAnEmptyObject(t *testing.T) {
	encoded, err := wire.MarshalMapFunc(map[string]shape(nil), encodeShape)
	if err != nil {
		t.Fatalf("MarshalMapFunc: %v", err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("a nil map encoded as %s, want {}", encoded)
	}
}

// Key order is sorted on the way out. A peer must not care, but a golden test, a
// transcript diff and a person reading a log all do.
func TestMapEncodingIsOrdered(t *testing.T) {
	values := map[string]shape{"c": &square{Side: 1}, "a": &square{Side: 1}, "b": &square{Side: 1}}
	encoded, err := wire.MarshalMapFunc(values, encodeShape)
	if err != nil {
		t.Fatalf("MarshalMapFunc: %v", err)
	}
	if got := strings.Index(string(encoded), `"a"`); got != 1 {
		t.Fatalf("encoded %s, want the keys in sorted order", encoded)
	}
}

// A value that does not decode fails the map and says which key, so a property
// name reaches the pointer the same way an array index does.
func TestAMapReportsTheKeyThatFailed(t *testing.T) {
	_, err := wire.UnmarshalMapFunc([]byte(`{"a":{"kind":"square"},"b":{"kind":"circle"}}`), decodeShape)
	if err == nil {
		t.Fatal("UnmarshalMapFunc accepted a value its decoder refused")
	}
	if !strings.Contains(err.Error(), "/b") {
		t.Errorf("the failure says %q, which does not name the key it happened at", err)
	}
}

// The value has to be an object. A map that is not one has no entries to decode,
// which is what lets the property-level fallback see the failure and recover.
func TestMapDecodingRequiresAnObject(t *testing.T) {
	for _, data := range []string{`null`, `"a"`, `[]`, `1`} {
		if _, err := wire.UnmarshalMapFunc([]byte(data), decodeShape); err == nil {
			t.Errorf("UnmarshalMapFunc(%q) accepted a value that is not an object", data)
		}
	}
}
