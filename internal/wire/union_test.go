package wire_test

import (
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// The discriminant goes first, because a reader of a protocol trace wants to know
// what a value is before reading it, and because a defined position is what makes
// golden output stable.
func TestTagObjectInsertsTheDiscriminantFirst(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "an object with properties",
			value: map[string]string{"text": "a"},
			want:  `{"type":"text","text":"a"}`,
		},
		{
			name:  "an empty object",
			value: struct{}{},
			want:  `{"type":"text"}`,
		},
		{
			name:  "an object whose encoder is custom",
			value: rawObject(`{"b":2,"a":1}`),
			want:  `{"type":"text","b":2,"a":1}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := wire.TagObject("type", "text", test.value)
			if err != nil {
				t.Fatalf("TagObject: %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("wrote %s, want %s", got, test.want)
			}
			if !json.Valid(got) {
				t.Fatalf("wrote invalid JSON: %s", got)
			}
		})
	}
}

// An arm that does not encode as an object cannot carry a discriminant, and
// splicing one in would produce a message no decoder could read.
func TestTagObjectRefusesANonObjectArm(t *testing.T) {
	for _, value := range []any{"a string", 1, []int{1}, nil} {
		if _, err := wire.TagObject("type", "text", value); err == nil {
			t.Errorf("TagObject accepted %#v, which does not encode as an object", value)
		}
	}
}

// A discriminant that is not a string is not a discriminant. That is the
// reference implementation's rule, and it decides arm selection: such a value
// falls through to the arms that declare no discriminant instead of matching the
// constant it resembles.
func TestTagIgnoresANonStringDiscriminant(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		tag   string
		found bool
	}{
		{name: "a string", data: `{"type":"text"}`, tag: "text", found: true},
		{name: "an empty string", data: `{"type":""}`, tag: "", found: true},
		{name: "a number", data: `{"type":12}`},
		{name: "null", data: `{"type":null}`},
		{name: "an object", data: `{"type":{"a":1}}`},
		{name: "absent", data: `{"other":1}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, err := wire.DecodeObject([]byte(test.data))
			if err != nil {
				t.Fatalf("DecodeObject: %v", err)
			}
			tag, found := wire.Tag(object, "type")
			if found != test.found || tag != test.tag {
				t.Fatalf("Tag = %q, %t; want %q, %t", tag, found, test.tag, test.found)
			}
		})
	}
}

// rawObject stands in for a generated encoder: it emits bytes of its own choosing
// in an order TagObject must not disturb.
type rawObject string

func (r rawObject) MarshalJSON() ([]byte, error) { return []byte(r), nil }
