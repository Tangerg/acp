package wire_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// The two array decoders differ in exactly one way, and it is the whole of
// x-deserialize-skip-invalid-items: one fails the message and the other drops the
// element.
func TestSliceDecodingSalvagesOnlyWhenAsked(t *testing.T) {
	const data = `["a",7,"b"]`

	if _, err := wire.UnmarshalSlice[string]([]byte(data)); err == nil {
		t.Error("UnmarshalSlice accepted an array with an element of the wrong type")
	}

	kept, err := wire.UnmarshalSliceSkippingInvalid[string]([]byte(data))
	if err != nil {
		t.Fatalf("UnmarshalSliceSkippingInvalid: %v", err)
	}
	if strings.Join(kept, ",") != "a,b" {
		t.Fatalf("kept %v, want [a b]", kept)
	}
}

// The keyword salvages elements, not the value they are in. An array that is not
// an array has no elements to salvage, so it is still a failure — which is what
// lets the property-level fallback see it and recover.
func TestSliceDecodingStillRequiresAnArray(t *testing.T) {
	for _, data := range []string{`null`, `"a"`, `{}`, `1`} {
		if _, err := wire.UnmarshalSliceSkippingInvalid[string]([]byte(data)); err == nil {
			t.Errorf("UnmarshalSliceSkippingInvalid(%q) accepted a value that is not an array", data)
		}
	}
}

// A strict failure names the element it happened at. Without that, a fifteen-arm
// union inside an array inside a request reports only that something was wrong.
func TestSliceDecodingReportsTheFailingIndex(t *testing.T) {
	_, err := wire.UnmarshalSlice[string]([]byte(`["a","b",7]`))
	if err == nil {
		t.Fatal("no error")
	}
	var pathErr *wire.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %v is not a PathError", err)
	}
	if pathErr.Path != "/2" {
		t.Fatalf("path = %q, want /2", pathErr.Path)
	}
}

// The Func variants exist for unions, whose arm cannot be selected by
// json.Unmarshal because a Go interface cannot decode into itself.
func TestSliceDecodingWithAnElementDecoder(t *testing.T) {
	decode := func(raw json.RawMessage) (int, error) {
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
		if value < 0 {
			return 0, errNegative
		}
		return value, nil
	}

	if _, err := wire.UnmarshalSliceFunc([]byte(`[1,-2,3]`), decode); err == nil {
		t.Error("UnmarshalSliceFunc accepted an element its decoder refused")
	}
	kept, err := wire.UnmarshalSliceFuncSkippingInvalid([]byte(`[1,-2,3]`), decode)
	if err != nil {
		t.Fatalf("UnmarshalSliceFuncSkippingInvalid: %v", err)
	}
	if len(kept) != 2 || kept[0] != 1 || kept[1] != 3 {
		t.Fatalf("kept %v, want [1 3]", kept)
	}
}

// A nil Go slice marshals as null, which is invalid wherever the schema requires
// an array — and a required property is exactly where a caller is most likely to
// have left the field alone.
func TestMarshalSliceWritesAnEmptyArrayForNil(t *testing.T) {
	got, err := wire.MarshalSlice[string](nil)
	if err != nil {
		t.Fatalf("MarshalSlice: %v", err)
	}
	if string(got) != `[]` {
		t.Fatalf("wrote %s, want []", got)
	}

	got, err = wire.MarshalSliceFunc(nil, func(string) ([]byte, error) { return nil, errNegative })
	if err != nil {
		t.Fatalf("MarshalSliceFunc: %v", err)
	}
	if string(got) != `[]` {
		t.Fatalf("wrote %s, want []", got)
	}
}

func TestMarshalSliceEncodesElementsInOrder(t *testing.T) {
	got, err := wire.MarshalSlice([]string{"a", "b"})
	if err != nil {
		t.Fatalf("MarshalSlice: %v", err)
	}
	if string(got) != `["a","b"]` {
		t.Fatalf("wrote %s", got)
	}
}

// UnmarshalValue's whole job beyond json.Unmarshal is refusing null, because
// json.Unmarshal accepts it silently: it leaves the Go value untouched and
// reports nothing, so a property the schema forbids to be null would arrive as
// its zero value.
func TestUnmarshalValueRefusesNull(t *testing.T) {
	_, err := wire.UnmarshalValue[string]([]byte(`null`))
	if !errors.Is(err, wire.ErrNotNullable) {
		t.Fatalf("error = %v, want ErrNotNullable", err)
	}

	value, err := wire.UnmarshalValue[string]([]byte(`"a"`))
	if err != nil || value != "a" {
		t.Fatalf("UnmarshalValue = %q, %v", value, err)
	}
}

var errNegative = errors.New("negative")
