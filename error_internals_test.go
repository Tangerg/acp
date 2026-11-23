package acp

import (
	"errors"
	"testing"

	"github.com/Tangerg/acp/jsonrpc"
)

func TestErrorCodeComparisonHandlesTypedNilErrors(t *testing.T) {
	var absent *Error
	failure := &Error{Code: ErrorCodeInvalidParams}
	if errors.Is(failure, absent) {
		t.Fatal("a real error matched a nil target")
	}
	if errors.Is(absent, failure) {
		t.Fatal("a nil error matched a real target")
	}
}

func errorResponse(raw string) *jsonrpc.Response {
	return &jsonrpc.Response{ID: jsonrpc.Int64ID(1), Error: []byte(raw)}
}

// The generated ErrorCode owns the int32 range, so a peer's error decodes through
// it and nothing else.
func TestAPeersErrorCodeMustFitThePublishedInt32Union(t *testing.T) {
	for _, code := range []string{"-2147483648", "-32042", "2147483647"} {
		var failure *Error
		if err := decodeResponse(errorResponse(`{"code":`+code+`,"message":"failed"}`), nil); !errors.As(err, &failure) {
			t.Errorf("code %s returned %v, want a preserved ACP error", code, err)
		}
	}
	for _, code := range []string{"-2147483649", "2147483648"} {
		err := decodeResponse(errorResponse(`{"code":`+code+`,"message":"failed"}`), nil)
		if failure := (*Error)(nil); errors.As(err, &failure) {
			t.Errorf("code %s decoded as %v, want a malformed-error failure", code, failure)
		}
	}
}

func TestAPeersErrorDataKeepsNullApartFromAbsent(t *testing.T) {
	var failure *Error
	if err := decodeResponse(errorResponse(`{"code":-32000,"message":"m","data": null }`), nil); !errors.As(err, &failure) {
		t.Fatalf("decode = %v", err)
	}
	if !failure.Data.IsNull() {
		t.Error("explicit null data was not preserved")
	}
	if err := decodeResponse(errorResponse(`{"code":-32000,"message":"m"}`), nil); !errors.As(err, &failure) {
		t.Fatalf("decode = %v", err)
	}
	if !failure.Data.IsZero() {
		t.Error("absent data became present")
	}
}
