package acp_test

import (
	"encoding/json"
	"testing"

	"github.com/Tangerg/acp"
)

// The constant is what goes on the wire in initialize, so the test states the
// number rather than importing it back out of the package under test: a test that
// reads its expectation from the code it checks passes however that code changes.
func TestProtocolVersionIsTheImplementedProtocolMajor(t *testing.T) {
	if acp.ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1", acp.ProtocolVersion)
	}
}

// initialize carries the version as a JSON number, not a string or a "v1" label.
// Encoding it here is the caller-side form of that promise.
func TestProtocolVersionEncodesAsJSONNumber(t *testing.T) {
	encoded, err := json.Marshal(struct {
		ProtocolVersion int `json:"protocolVersion"`
	}{acp.ProtocolVersion})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"protocolVersion":1}`; got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
}
