package acp

import (
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp/internal/jsonrpc2"
)

func TestWireErrorCodeMustFitThePublishedInt32Union(t *testing.T) {
	for _, code := range []int64{-1 << 31, -32042, 1<<31 - 1} {
		var failure *Error
		if err := errorFromWire(&jsonrpc2.WireError{Code: code, Message: "failed"}); !errors.As(err, &failure) {
			t.Errorf("code %d returned %v, want a preserved ACP error", code, err)
		} else if int64(failure.Code) != code {
			t.Errorf("code %d became %d", code, failure.Code)
		}
	}
	for _, code := range []int64{-1<<31 - 1, 1 << 31} {
		if err := errorFromWire(&jsonrpc2.WireError{Code: code}); err == nil || !strings.Contains(
			err.Error(), "outside ACP's int32 range",
		) {
			t.Errorf("code %d returned %v, want an int32 range error", code, err)
		}
	}
}
