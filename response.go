package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// Responses use a connection-owned deadline because nobody is waiting locally
// to bound a peer that stopped reading.
const responseWriteTimeout = 5 * time.Second

func (l *link) respond(id jsonrpc.ID, result any, handlerErr error) bool {
	claim, claimed := l.requests.claim(id)
	if !claimed {
		return false
	}
	if claim.cancelled && (result == nil || handlerErr != nil) {
		// Once this implementation acts on $/cancel_request, the published
		// protocol permits only a valid result or -32800 for the original call.
		result = nil
		handlerErr = newError(ErrorCodeRequestCancelled, "request cancelled")
	}
	return l.writeResponse(id, result, handlerErr)
}

// The connection context owns response delivery because a cancelled request
// still requires exactly one JSON-RPC response.
func (l *link) writeResponse(id jsonrpc.ID, result any, handlerErr error) bool {
	if result == nil && handlerErr == nil {
		handlerErr = newError(ErrorCodeInternalError, "no result and no error")
	}
	response, err := jsonrpc2.NewResponse(id, result, l.wireError(handlerErr))
	if err != nil {
		l.logger.Error("acp: encoding a result failed", slog.Any("error", err))
		response, err = jsonrpc2.NewResponse(id, nil,
			l.wireError(newError(ErrorCodeInternalError, "the result could not be encoded")))
		if err != nil {
			l.endReading(err)
			return false
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), responseWriteTimeout)
	defer cancel()
	if err := l.transport.Write(ctx, response); err != nil {
		l.logger.Error("acp: writing a response failed", slog.Any("error", err))
		l.endReading(err)
		return false
	}
	return true
}

// Handler details stay local unless the handler deliberately returned an ACP
// Error, because arbitrary errors routinely contain paths and host identifiers.
func (l *link) wireError(err error) *jsonrpc2.WireError {
	if err == nil {
		return nil
	}
	var chosen *Error
	if errors.As(err, &chosen) {
		return chosen.toWire()
	}
	l.logger.Error("acp: a handler failed", slog.Any("error", err))
	return newError(ErrorCodeInternalError, "%s", ErrorCodeInternalError).toWire()
}

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
