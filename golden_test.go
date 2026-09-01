package acp_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Tangerg/acp"
)

// These are the encoder's own promises, for values built in Go rather than
// decoded from a peer. The cross-SDK corpus cannot state them: it starts from
// JSON, so it can never construct the nil slice, the absent Opt or the
// out-of-double-range integer that this checks.
//
// They are exact bytes on purpose. Key order, the presence or absence of a
// property and the spelling of a number are what a reader of a protocol trace
// sees, and a semantic comparison would let all three drift.
func TestGoldenEncoding(t *testing.T) {
	tests := []struct {
		name  string
		why   string
		value any
		want  string
	}{
		{
			name: "properties are emitted in schema order",
			why: "a message should be readable against the schema that defines it, and golden output " +
				"should not move when an unrelated field is added",
			value: &acp.TextContent{Text: "hi"},
			want:  `{"text":"hi"}`,
		},
		{
			name:  "a nil required slice encodes as an empty array",
			why:   "a nil Go slice marshals as null, which is invalid where the schema requires an array",
			value: &acp.PromptRequest{SessionID: "sess-1"},
			want:  `{"sessionId":"sess-1","prompt":[]}`,
		},
		{
			name:  "an absent optional property is omitted",
			why:   "the zero Opt is the absent state, and absent means the property is not in the message",
			value: &acp.EnumOption{Const: "a", Title: "A"},
			want:  `{"const":"a","title":"A"}`,
		},
		{
			name:  "a null optional property is emitted as null",
			why:   "present-as-null is a state of its own, and omitting it would lose the distinction",
			value: &acp.EnumOption{Const: "a", Title: "A", Description: acp.OptNull[string]()},
			want:  `{"const":"a","title":"A","description":null}`,
		},
		{
			name:  "a present optional property is emitted",
			why:   "the third state",
			value: &acp.EnumOption{Const: "a", Title: "A", Description: acp.OptValue("why")},
			want:  `{"const":"a","title":"A","description":"why"}`,
		},
		{
			name: "an empty present array is not an absent one",
			why: "a nil optional slice is absent and omitted, but an empty one is present; omitempty " +
				"cannot express the difference and omitzero can",
			value: &acp.NewSessionRequest{Cwd: "/w", AdditionalDirectories: []string{}},
			want:  `{"cwd":"/w","additionalDirectories":[],"mcpServers":[]}`,
		},
		{
			name:  "a nil optional slice is omitted",
			why:   "the other half of the same distinction",
			value: &acp.NewSessionRequest{Cwd: "/w"},
			want:  `{"cwd":"/w","mcpServers":[]}`,
		},
		{
			name: "a union arm carries the discriminant the union owns",
			why: "the arm type does not write its own tag — ContentChunk is the payload of three " +
				"different SessionUpdate arms — so the union splices it in, first",
			value: &acp.PromptRequest{
				SessionID: "sess-1",
				Prompt: []acp.ContentBlock{
					&acp.TextContent{Text: "a"},
					&acp.ResourceLink{Name: "n", URI: "file:///a"},
				},
			},
			want: `{"sessionId":"sess-1","prompt":[{"type":"text","text":"a"},` +
				`{"type":"resource_link","name":"n","uri":"file:///a"}]}`,
		},
		{
			name:  "an untagged arm carries no discriminant",
			why:   "the schema gives this arm no constant, and inventing one would be a local dialect",
			value: &acp.EmbeddedResource{Resource: &acp.TextResourceContents{URI: "file:///a", Text: "t"}},
			want:  `{"resource":{"text":"t","uri":"file:///a"}}`,
		},
		{
			name: "a catch-all arm's retained properties are emitted in sorted order",
			why: "the properties came from a peer, so they have no schema order to follow; sorting is " +
				"what makes one Go value encode the same way every time",
			value: &acp.MultiSelectItemsOther{
				Type: "_vendor",
				Extra: map[string]json.RawMessage{
					"zebra": json.RawMessage(`1`),
					"apple": json.RawMessage(`{"k":[true,null]}`),
				},
			},
			want: `{"type":"_vendor","apple":{"k":[true,null]},"zebra":1}`,
		},
		{
			name: "an integer beyond a double's precision survives",
			why: "the Go type comes from the schema's format, so an int64 stays an int64; this is the " +
				"one place the two SDKs cannot agree, and the corpus deliberately does not ask them to",
			value: &acp.ResourceLink{
				Name: "n",
				URI:  "file:///a",
				Size: acp.OptValue(int64(math.MaxInt64)),
			},
			want: `{"name":"n","size":9223372036854775807,"uri":"file:///a"}`,
		},
		{
			name:  "an empty object is an object",
			why:   "a type all of whose properties are optional still encodes as {}, never as null",
			value: &acp.Annotations{},
			want:  `{}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != test.want {
				t.Fatalf("encoded\n  %s\nwant\n  %s\n(%s)", encoded, test.want, test.why)
			}
		})
	}
}

// A value that came off the wire and one built in Go must encode alike. The
// golden table above only checks the second, and a decoder that quietly recorded
// something extra would pass it.
func TestGoldenEncodingSurvivesADecode(t *testing.T) {
	const message = `{"sessionId":"sess-1","prompt":[{"type":"text","text":"a"}],"_meta":{"k":1}}`

	var decoded acp.PromptRequest
	if err := json.Unmarshal([]byte(message), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	encoded, err := json.Marshal(&decoded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != message {
		t.Fatalf("re-encoded\n  %s\nwant\n  %s", encoded, message)
	}
}

// The encoder refuses what it cannot express, rather than emitting something the
// peer will reject. A nil arm in a union of interfaces is the case that matters:
// a caller who forgot to set one would otherwise send null inside an array of
// objects.
func TestEncodingRefusesANilUnionArm(t *testing.T) {
	_, err := json.Marshal(&acp.PromptRequest{
		SessionID: "sess-1",
		Prompt:    []acp.ContentBlock{nil},
	})
	if err == nil {
		t.Fatal("encoded a nil content block; the peer would have received null where an object is required")
	}
}

// A closed enumeration is checked on the way out too. Decoding cannot produce an
// undefined value, so without this the only way to send one would be to build it
// in Go — which is exactly what a caller does.
func TestEncodingRefusesAnUndefinedEnumerationValue(t *testing.T) {
	if _, err := json.Marshal(acp.StopReason("gave_up")); err == nil {
		t.Fatal("encoded a StopReason the schema does not define")
	}
	if _, err := json.Marshal(acp.StopReasonEndTurn); err != nil {
		t.Fatalf("refused a defined StopReason: %v", err)
	}
}

// An open string union has no such check, and must not acquire one: the schema's
// last arm is a bare string, so a value outside the constants is valid.
func TestEncodingKeepsAnUnlistedOpenUnionValue(t *testing.T) {
	encoded, err := json.Marshal(acp.SessionConfigOptionCategory("_house_brand"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `"_house_brand"`; got != want {
		t.Fatalf("encoded %s, want %s", got, want)
	}
}

// The catch-all arm's `not` clause is a rule in both directions. Sending a
// reserved discriminant with a payload that does not match the arm claiming it
// would be a message this package's own decoder refuses.
func TestEncodingRefusesAReservedCatchAllDiscriminant(t *testing.T) {
	_, err := json.Marshal(&acp.MultiSelectItemsOther{Type: "string"})
	if err == nil {
		t.Fatal(`encoded a catch-all arm tagged "string", which a known arm reserves`)
	}
}
