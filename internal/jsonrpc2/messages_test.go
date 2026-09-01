package jsonrpc2

import (
	"bytes"
	"strings"
	"testing"
)

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
	if !request.IsCall() || request.ID.Raw() != nil {
		t.Fatalf("explicit null became %#v", request.ID)
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
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":-9223372036854775808,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":9223372036854775807,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":1.0,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":1e3,"method":"_test"}`,
	} {
		if _, err := DecodeMessage([]byte(input)); err != nil {
			t.Errorf("DecodeMessage(%s): %v", input, err)
		}
	}

	for _, input := range []string{
		`{"jsonrpc":"2.0","id":1.5,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":1.0000000000000000001,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":1e100,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":9223372036854775808,"method":"_test"}`,
		`{"jsonrpc":"2.0","id":-9223372036854775809,"method":"_test"}`,
	} {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted an ID outside the ACP RequestId union", input)
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

	for _, input := range []string{
		`{"jsonrpc":"2.0","method":null}`,
		`{"jsonrpc":"2.0","method":1}`,
		`null`,
	} {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted a non-string method or non-object envelope", input)
		}
	}
}

func TestResponseEncoderRefusesInvalidEnvelopes(t *testing.T) {
	id := Int64ID(1)
	for name, response := range map[string]*Response{
		"absent id": {Result: []byte(`{}`)},
		"both result and error": {
			ID:     id,
			Result: []byte(`{}`),
			Error:  NewError(-32603, "failed"),
		},
		"neither result nor error": {ID: id},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeMessage(response); err == nil {
				t.Fatal("EncodeMessage accepted an invalid response envelope")
			}
		})
	}
}

func TestConstructorsRefuseMissingEnvelopeObligations(t *testing.T) {
	if _, err := NewCall(ID{}, "_test", nil); err == nil {
		t.Fatal("NewCall accepted an absent request identifier")
	}
	if _, err := NewResponse(ID{}, struct{}{}, nil); err == nil {
		t.Fatal("NewResponse accepted an absent request identifier")
	}
	if _, err := NewResponse(Int64ID(1), nil, nil); err == nil {
		t.Fatal("NewResponse accepted neither a result nor an error")
	}
	if _, err := NewResponse(Int64ID(1), struct{}{}, NewError(-32603, "failed")); err == nil {
		t.Fatal("NewResponse accepted both a result and an error")
	}
}

func TestDecodeMessageMatchesProtocolFieldNamesExactly(t *testing.T) {
	valid := []string{
		`{"jsonrpc":"2.0","method":"_test"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"failed"}}`,
	}
	for _, input := range valid {
		if _, err := DecodeMessage([]byte(input)); err != nil {
			t.Errorf("DecodeMessage(%s): %v", input, err)
		}
	}

	invalid := []string{
		`{"JSONRPC":"2.0","method":"_test"}`,
		`{"jsonrpc":"2.0","Method":"_test"}`,
		`{"jsonrpc":"2.0","id":1,"Result":{}}`,
		`{"jsonrpc":"2.0","id":1,"Error":{"code":-32603,"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"Code":-32603,"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"Message":"failed"}}`,
		`{"jsonrpc\u0000":"2.0","method":"_test"}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code\u0000":-32603,"message":"failed"}}`,
	}
	for _, input := range invalid {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted a case-insensitive field match", input)
		}
	}
}

func TestDecodeMessageRefusesMalformedResponses(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32603,"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
		`{"jsonrpc":"2.0","id":1,"error":{"message":"failed"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603}}`,
	} {
		if _, err := DecodeMessage([]byte(input)); err == nil {
			t.Errorf("DecodeMessage(%s) accepted a malformed response", input)
		}
	}
}

func TestDecodeMessageRefusesTrailingJSONValues(t *testing.T) {
	if _, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"_test"} {}`)); err == nil {
		t.Fatal("DecodeMessage accepted a second JSON value")
	}
}

// ACP envelopes are shallow. This deliberately avoids pinning encoding/json's
// current numeric limit; it guards the security property that an implementation
// swap must not make hostile nesting unbounded.
func TestDecodeMessageRefusesPathologicalNesting(t *testing.T) {
	const pathologicalDepth = 100_000

	data := []byte(`{"jsonrpc":"2.0","method":"_test","params":` +
		strings.Repeat("[", pathologicalDepth) + `0` +
		strings.Repeat("]", pathologicalDepth) + `}`)
	if _, err := DecodeMessage(data); err == nil {
		t.Fatal("DecodeMessage accepted a pathologically nested message")
	}
}
