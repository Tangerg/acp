package wire_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// DecodeObject exists so that one property can fail on its own, and that only
// works if it refuses the values a plain decode into a map accepts silently. null
// is the one that matters: unmarshalling it into a map leaves the map alone and
// reports nothing.
func TestDecodeObjectRefusesWhatIsNotAnObject(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "object", data: `{"a":1}`, want: true},
		{name: "empty object", data: `{}`, want: true},
		{name: "object with leading space", data: "  {\"a\":1}", want: true},
		{name: "null", data: `null`},
		{name: "array", data: `[]`},
		{name: "string", data: `"a"`},
		{name: "number", data: `1`},
		{name: "boolean", data: `true`},
		{name: "nothing", data: ``},
		{name: "truncated", data: `{"a":`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := wire.DecodeObject([]byte(test.data))
			if (err == nil) != test.want {
				t.Fatalf("DecodeObject(%q) error = %v, want error: %t", test.data, err, !test.want)
			}
		})
	}
}

// The whole point of splitting an object is that the values stay undecoded, so a
// property that fails to decode can be replaced without the rest of the message
// noticing.
func TestDecodeObjectKeepsValuesRaw(t *testing.T) {
	object, err := wire.DecodeObject([]byte(`{"a":{"nested":[1,2]},"b":null}`))
	if err != nil {
		t.Fatalf("DecodeObject: %v", err)
	}
	if got, want := string(object["a"]), `{"nested":[1,2]}`; got != want {
		t.Errorf("object[a] = %s, want %s", got, want)
	}
	if _, present := object["b"]; !present {
		t.Error("a property present as null is reported absent")
	}
	if !wire.IsNull(object["b"]) {
		t.Error("IsNull does not recognise a null property")
	}
}

func TestObjectWriterKeepsTheOrderPropertiesAreSet(t *testing.T) {
	var writer wire.ObjectWriter
	writer.Set("zebra", 1)
	writer.Set("apple", "two")
	writer.SetRaw("nested", []byte(`{"k":[true]}`))

	got, err := writer.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	want := `{"zebra":1,"apple":"two","nested":{"k":[true]}}`
	if string(got) != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
}

// An object with no properties is still an object. A writer that returned an
// empty buffer would produce invalid JSON at exactly the point where a type all
// of whose properties are optional is encoded.
func TestObjectWriterWritesAnEmptyObject(t *testing.T) {
	var writer wire.ObjectWriter
	got, err := writer.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != `{}` {
		t.Fatalf("wrote %s, want {}", got)
	}
}

func TestObjectWriterOmitsAnAbsentOptional(t *testing.T) {
	var writer wire.ObjectWriter
	writer.SetOptional("absent", stubOptional{absent: true})
	writer.SetOptional("null", stubOptional{encoded: "null"})
	writer.Set("after", 1)

	got, err := writer.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	want := `{"null":null,"after":1}`
	if string(got) != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
}

// A property name that came from a peer needs escaping, and so does a value. The
// writer builds bytes by hand, so this is the check that it does not build
// invalid ones.
func TestObjectWriterEscapes(t *testing.T) {
	var writer wire.ObjectWriter
	writer.Set(`a "quoted" name`, "a \"quoted\" value\n")

	got, err := writer.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("the writer produced invalid JSON %s: %v", got, err)
	}
	if decoded[`a "quoted" name`] != "a \"quoted\" value\n" {
		t.Fatalf("round-tripped %q", decoded)
	}
}

// Retained properties came from a peer, so they have no schema order to follow.
// Sorting them is what makes one Go value encode the same way every time.
func TestObjectWriterSortsRetainedProperties(t *testing.T) {
	var writer wire.ObjectWriter
	writer.Set("type", "_vendor")
	writer.SetExtra(map[string]json.RawMessage{
		"zebra": json.RawMessage(`1`),
		"apple": json.RawMessage(`2`),
		"mango": json.RawMessage(`3`),
	})

	got, err := writer.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	want := `{"type":"_vendor","apple":2,"mango":3,"zebra":1}`
	if string(got) != want {
		t.Fatalf("wrote %s, want %s", got, want)
	}
}

// Unlike SetRaw, SetExtra validates: these values reached the struct from a peer
// or from a caller rather than from a generated encoder, and one invalid value
// would make the whole message unparseable at the far end.
func TestObjectWriterRefusesInvalidRetainedJSON(t *testing.T) {
	var writer wire.ObjectWriter
	writer.SetExtra(map[string]json.RawMessage{"broken": json.RawMessage(`{`)})

	if _, err := writer.Bytes(); err == nil {
		t.Fatal("accepted a retained property that is not valid JSON")
	}
}

// A failure is kept and reported once, from Bytes, so a generated encoder does
// not have to check every property. The property it happened at still has to be
// identifiable.
func TestObjectWriterReportsTheFailingProperty(t *testing.T) {
	var writer wire.ObjectWriter
	writer.Set("fine", 1)
	writer.Set("broken", failingMarshaler{})
	writer.Set("also fine", 2)

	_, err := writer.Bytes()
	if err == nil {
		t.Fatal("Bytes reported no error")
	}
	var pathErr *wire.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error %v is not a PathError", err)
	}
	if pathErr.Path != "/broken" {
		t.Fatalf("path = %q, want /broken", pathErr.Path)
	}
}

func TestExtraDropsDeclaredProperties(t *testing.T) {
	object := map[string]json.RawMessage{
		"type":  json.RawMessage(`"_vendor"`),
		"extra": json.RawMessage(`1`),
	}
	extra := wire.Extra(object, "type")
	if len(extra) != 1 {
		t.Fatalf("Extra kept %d properties, want 1", len(extra))
	}
	if _, present := extra["extra"]; !present {
		t.Error("Extra dropped an undeclared property")
	}

	// Nil rather than an empty map, so that a value with no retained properties
	// encodes identically to one whose map was never touched.
	if wire.Extra(object, "type", "extra") != nil {
		t.Error("Extra returned a non-nil map with nothing in it")
	}
}

func TestRawItemsRefusesWhatIsNotAnArray(t *testing.T) {
	for _, data := range []string{`null`, `{}`, `"a"`, `1`} {
		if _, err := wire.RawItems([]byte(data)); err == nil {
			t.Errorf("RawItems(%q) accepted a value that is not an array", data)
		}
	}
	items, err := wire.RawItems([]byte(`[1,{"a":2}]`))
	if err != nil {
		t.Fatalf("RawItems: %v", err)
	}
	if len(items) != 2 || string(items[1]) != `{"a":2}` {
		t.Fatalf("items = %v", items)
	}
}

type stubOptional struct {
	absent  bool
	encoded string
}

func (s stubOptional) IsZero() bool { return s.absent }

func (s stubOptional) MarshalJSON() ([]byte, error) { return []byte(s.encoded), nil }

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) { return nil, errStub }

var errStub = errors.New("stub failure")

// A catch-all arm's alternatives are told apart by which properties they require,
// and those are retained rather than declared, so the check reads the map the arm
// kept them in.
func TestSatisfiesOneGroup(t *testing.T) {
	scope := [][]string{{"sessionId"}, {"requestId"}}
	both := [][]string{{"a", "b"}}

	tests := []struct {
		name   string
		extra  map[string]json.RawMessage
		groups [][]string
		want   bool
		why    string
	}{
		{
			name:   "no groups",
			extra:  nil,
			groups: nil,
			want:   true,
			why:    "an arm that is not a union is satisfied by everything",
		},
		{
			name:   "the first alternative",
			extra:  map[string]json.RawMessage{"sessionId": json.RawMessage(`"s"`)},
			groups: scope,
			want:   true,
		},
		{
			name:   "the second alternative",
			extra:  map[string]json.RawMessage{"requestId": json.RawMessage(`7`)},
			groups: scope,
			want:   true,
		},
		{
			name:   "neither alternative",
			extra:  map[string]json.RawMessage{"channel": json.RawMessage(`"alpha"`)},
			groups: scope,
			want:   false,
			why:    "a custom mode still has to say whether it belongs to a session or to a request",
		},
		{
			name:   "nothing retained at all",
			extra:  nil,
			groups: scope,
			want:   false,
		},
		{
			name:   "a group only partly satisfied",
			extra:  map[string]json.RawMessage{"a": json.RawMessage(`1`)},
			groups: both,
			want:   false,
			why:    "an alternative requiring two properties is not satisfied by one of them",
		},
		{
			name:   "a group fully satisfied",
			extra:  map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`2`)},
			groups: both,
			want:   true,
		},
		{
			name:   "a null value still counts as present",
			extra:  map[string]json.RawMessage{"sessionId": json.RawMessage(`null`)},
			groups: scope,
			want:   true,
			why:    "the alternatives are told apart by which keys are there, and whether a value is legal is the property's own question",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wire.SatisfiesOneGroup(test.extra, test.groups); got != test.want {
				t.Fatalf("SatisfiesOneGroup = %t, want %t (%s)", got, test.want, test.why)
			}
		})
	}
}
