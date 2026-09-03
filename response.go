package acp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// Envelope validation precedes payload decoding so contradictory or empty
// responses cannot masquerade as a zero-valued successful result.
func decodeResponse(response *jsonrpc.Response, result any) error {
	if response.Error != nil {
		if len(response.Result) > 0 {
			return fmt.Errorf("acp: the peer answered with both a result and an error: %w",
				response.Error)
		}
		return errorFromWire(response.Error)
	}
	if len(response.Result) == 0 {
		return errors.New("acp: the peer answered with neither a result nor an error")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

func requestIDOf(id jsonrpc.ID) requestID {
	switch value := id.Raw().(type) {
	case string:
		arm := requestIDStr(value)
		return &arm
	case int64:
		arm := requestIDNumber(value)
		return &arm
	default:
		return &requestIDNull{}
	}
}

// Every schema-defined arm can name a call, including the discouraged null one.
// An implementation outside the union cannot.
func jsonrpcID(id requestID) (jsonrpc.ID, bool) {
	switch arm := id.(type) {
	case *requestIDStr:
		return jsonrpc2.StringID(string(*arm)), true
	case *requestIDNumber:
		return jsonrpc2.Int64ID(int64(*arm)), true
	case *requestIDNull:
		return jsonrpc2.NullID(), true
	default:
		return jsonrpc.ID{}, false
	}
}
