package acp_test

import (
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp"
)

// The three states are the reason this type exists, so they are stated as a
// property of a real generated field rather than of Opt on its own: the claim is
// that a message round-trips through them, not that a wrapper has three
// constructors.
func TestThreeStatesSurviveARoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		message string
		absent  bool
		null    bool
		value   string
	}{
		{
			name:    "absent",
			message: `{"const":"a","title":"A"}`,
			absent:  true,
		},
		{
			name:    "null",
			message: `{"const":"a","title":"A","description":null}`,
			null:    true,
		},
		{
			name:    "present",
			message: `{"const":"a","title":"A","description":"why"}`,
			value:   "why",
		},
		{
			name:    "present and empty",
			message: `{"const":"a","title":"A","description":""}`,
			value:   "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var option acp.EnumOption
			if err := json.Unmarshal([]byte(test.message), &option); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got := option.Description.IsZero(); got != test.absent {
				t.Errorf("IsZero() = %t, want %t", got, test.absent)
			}
			if got := option.Description.IsNull(); got != test.null {
				t.Errorf("IsNull() = %t, want %t", got, test.null)
			}
			value, present := option.Description.Get()
			if want := !test.absent && !test.null; present != want {
				t.Errorf("Get() present = %t, want %t", present, want)
			}
			if present && value != test.value {
				t.Errorf("Get() value = %q, want %q", value, test.value)
			}

			// The states are only worth keeping apart if they survive being sent
			// on again, which is what an editor relaying a message does.
			encoded, err := json.Marshal(&option)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != test.message {
				t.Fatalf("re-encoded %s, want %s", encoded, test.message)
			}
		})
	}
}

// The zero value is the absent state. Everything else — the omitzero tag on
// every generated Opt field, and a struct literal that leaves a property out —
// depends on that being true rather than on a constructor being called.
func TestTheZeroOptIsAbsent(t *testing.T) {
	var absent acp.Opt[string]
	if !absent.IsZero() {
		t.Error("the zero Opt is not absent")
	}
	if absent.IsNull() {
		t.Error("the zero Opt is null")
	}
	if _, present := absent.Get(); present {
		t.Error("the zero Opt has a value")
	}
}

// Null and absent are different states of the same field, and a type that
// answered yes to both would make the distinction unusable.
func TestNullIsNotAbsent(t *testing.T) {
	null := acp.OptNull[string]()
	if null.IsZero() {
		t.Error("a null Opt reports itself absent, so omitzero would drop it")
	}
	if !null.IsNull() {
		t.Error("a null Opt does not report itself null")
	}
	if _, present := null.Get(); present {
		t.Error("a null Opt reports a value")
	}
}

// A present Opt holding the zero value of its type is present. This is the case
// omitempty cannot express and is why the generated fields use omitzero.
func TestAPresentZeroValueIsPresent(t *testing.T) {
	present := acp.OptValue("")
	if present.IsZero() {
		t.Error("a present empty string reports itself absent")
	}
	value, ok := present.Get()
	if !ok || value != "" {
		t.Errorf("Get() = %q, %t; want \"\", true", value, ok)
	}
}

// An Opt whose value type is a struct nests the same way, which is what the
// generated optional object properties rely on.
func TestOptHoldsAStructValue(t *testing.T) {
	const message = `{"type":"text","text":"a","annotations":{"priority":0.5}}`

	block, err := decodeContentBlock(message)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	text, ok := block.(*acp.TextContent)
	if !ok {
		t.Fatalf("decoded %T, want *acp.TextContent", block)
	}
	annotations, present := text.Annotations.Get()
	if !present {
		t.Fatal("annotations are absent")
	}
	priority, present := annotations.Priority.Get()
	if !present || priority != 0.5 {
		t.Fatalf("priority = %v, %t; want 0.5, true", priority, present)
	}
}

// decodeContentBlock reaches a union arm through the type that carries it, which
// is the only way an importer can: arm selection belongs to the union's codec and
// is not exported.
func decodeContentBlock(block string) (acp.ContentBlock, error) {
	var request acp.PromptRequest
	message := `{"sessionId":"s","prompt":[` + block + `]}`
	if err := json.Unmarshal([]byte(message), &request); err != nil {
		return nil, err
	}
	return request.Prompt[0], nil
}
