package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEncoderRefusesInvalidRawJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		data json.RawMessage
	}{
		{"malformed", json.RawMessage(`{`)},
		{"empty", json.RawMessage{}},
		{"invalid UTF-8", json.RawMessage{'"', 0xff, '"'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := Int64ID(1)
			for _, message := range []Message{
				&Request{ID: id, Method: "_test", Params: test.data},
				&Response{ID: id, Result: test.data},
				&Response{ID: id, Error: test.data},
			} {
				if _, err := EncodeMessage(message); err == nil {
					t.Errorf("encoder accepted invalid raw data in %T", message)
				}
			}
		})
	}
}

func TestRequestIDKeepsAbsentAndNullSeparate(t *testing.T) {
	notification, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"_test"}`))
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if notification.(*Request).IsCall() {
		t.Fatal("an absent identifier became a call")
	}
	encoded, err := EncodeMessage(notification)
	if err != nil {
		t.Fatalf("encode notification: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"id"`)) {
		t.Fatalf("an absent identifier was emitted: %s", encoded)
	}

	call, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":null,"method":"_test"}`))
	if err != nil {
		t.Fatalf("decode null identifier: %v", err)
	}
	request := call.(*Request)
	if !request.IsCall() || request.ID != NullID() {
		t.Fatalf("explicit null became %v", request.ID)
	}
	encoded, err = EncodeMessage(request)
	if err != nil {
		t.Fatalf("encode null identifier: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"id":null`)) {
		t.Fatalf("explicit null was not preserved: %s", encoded)
	}
}

func TestRequestIDAcceptsExactlyInt64Numbers(t *testing.T) {
	for input, want := range map[string]ID{
		`-9223372036854775808`: Int64ID(-9223372036854775808),
		`9223372036854775807`:  Int64ID(9223372036854775807),
		`1.0`:                  Int64ID(1),
		`1e3`:                  Int64ID(1000),
		`"1"`:                  StringID("1"),
	} {
		message, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":` + input + `,"method":"_test"}`))
		if err != nil {
			t.Errorf("id %s: %v", input, err)
			continue
		}
		if got := message.(*Request).ID; got != want {
			t.Errorf("id %s decoded as %v, want %v", input, got, want)
		}
	}

	for _, input := range []string{
		`1.5`, `1.0000000000000000001`, `1e100`, `9223372036854775808`,
		`-9223372036854775809`, `{}`, `true`,
	} {
		_, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":` + input + `,"method":"_test"}`))
		var decodeErr *DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.Syntax {
			t.Errorf("id %s: want an invalid-request DecodeError, got %v", input, err)
		}
	}
}

func TestMethodPresenceDeterminesRequestShape(t *testing.T) {
	message, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","method":""}`))
	if err != nil {
		t.Fatalf("decode empty method: %v", err)
	}
	request, ok := message.(*Request)
	if !ok || request.Method != "" || request.IsCall() {
		t.Fatalf("empty method became %#v, want a notification request", message)
	}
	encoded, err := EncodeMessage(request)
	if err != nil {
		t.Fatalf("encode empty method: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"method":""`)) {
		t.Fatalf("empty method lost its request shape: %s", encoded)
	}

	for _, input := range []string{`{"jsonrpc":"2.0","method":null}`, `{"jsonrpc":"2.0","method":1}`, `null`} {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted a non-string method or non-object envelope", input)
		}
	}
}

func TestResponseEncoderRefusesInvalidEnvelopes(t *testing.T) {
	id := Int64ID(1)
	for name, response := range map[string]*Response{
		"absent id":                {Result: []byte(`{}`)},
		"both result and error":    {ID: id, Result: []byte(`{}`), Error: []byte(`{"code":-32603,"message":"x"}`)},
		"neither result nor error": {ID: id},
		"error not an object":      {ID: id, Error: []byte(`"failed"`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeMessage(response); err == nil {
				t.Fatal("EncodeMessage accepted an invalid response envelope")
			}
		})
	}
}

func TestDecodeMessageMatchesEnvelopeMembersExactly(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","method":"_test"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"failed"}}`,
	} {
		if _, err := DecodeMessage([]byte(input)); err != nil {
			t.Errorf("DecodeMessage(%s): %v", input, err)
		}
	}
	for _, input := range []string{
		`{"JSONRPC":"2.0","method":"_test"}`,
		`{"jsonrpc":"2.0","Method":"_test"}`,
		`{"jsonrpc":"2.0","id":1,"Result":{}}`,
		`{"jsonrpc":"2.0","id":1,"Error":{"code":-32603,"message":"failed"}}`,
		`{"jsonrpc\u0000":"2.0","method":"_test"}`,
	} {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted a case-insensitive member match", input)
		}
	}
}

// A malformed response whose identifier is readable names the call it fails, so
// that call can stop waiting.
func TestMalformedResponsesNameTheirCall(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":7}`,
		`{"jsonrpc":"2.0","id":7,"result":{},"error":{"code":-32603,"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":7,"error":null}`,
		`{"jsonrpc":"2.0","id":7,"error":"failed"}`,
	} {
		_, err := DecodeMessage([]byte(input))
		var decodeErr *DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.ResponseID != Int64ID(7) {
			t.Errorf("DecodeMessage(%s) = %v, want a DecodeError naming id 7", input, err)
		}
	}
}

func TestSyntaxErrorsAreDistinguishedFromInvalidEnvelopes(t *testing.T) {
	for input, syntax := range map[string]bool{
		`not json`:                          true,
		`{"jsonrpc":"2.0"`:                  true,
		`{"jsonrpc":"2.0","method":"_"} {}`: true,
		"\xff":                              true,
		`[]`:                                false,
		`42`:                                false,
		`{"jsonrpc":"1.0","method":"_"}`:    false,
	} {
		_, err := DecodeMessage([]byte(input))
		var decodeErr *DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.Syntax != syntax {
			t.Errorf("DecodeMessage(%q) = %v, want a DecodeError with Syntax %t", input, err, syntax)
		}
	}
}

// ACP envelopes are shallow. This guards the property that hostile nesting stays
// bounded without pinning encoding/json's current limit.
func TestDecodeMessageRefusesPathologicalNesting(t *testing.T) {
	const pathologicalDepth = 100_000

	data := []byte(`{"jsonrpc":"2.0","method":"_test","params":` +
		strings.Repeat("[", pathologicalDepth) + `0` +
		strings.Repeat("]", pathologicalDepth) + `}`)
	if _, err := DecodeMessage(data); err == nil {
		t.Fatal("DecodeMessage accepted a pathologically nested message")
	}
}
