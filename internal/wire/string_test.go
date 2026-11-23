package wire_test

import (
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

func TestStringLengthCountsCodePoints(t *testing.T) {
	constraints := wire.StringConstraints{MinLength: 2}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"", false},
		{"a", false},
		{"界", false},
		{"🦀", false},
		{"界面", true},
		{"e\u0301", true},
	} {
		t.Run(test.value, func(t *testing.T) {
			encoded, err := constraints.Encode(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("Encode(%q) = %s, %v", test.value, encoded, err)
			}
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := constraints.Decode(raw)
			if (err == nil) != test.valid || test.valid && decoded != test.value {
				t.Fatalf("Decode(%s) = %q, %v", raw, decoded, err)
			}
		})
	}
}

func TestStringConstraintsCompose(t *testing.T) {
	constraints := wire.StringConstraints{MinLength: 5, AbsoluteURI: true}
	for _, value := range []string{"abc:", "a long relative path"} {
		if err := constraints.Validate(value); err == nil {
			t.Fatalf("accepted %q despite a failed constraint", value)
		}
	}
	if err := constraints.Validate("urn:example"); err != nil {
		t.Fatal(err)
	}
}
