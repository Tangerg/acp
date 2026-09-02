package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tangerg/acp/jsonrpc"
)

// These helpers keep decoding and handler failures identical across methods.
func dispatchCall[Request, Response any](
	ctx context.Context,
	request *jsonrpc.Request,
	handle func(context.Context, *Request) (*Response, error),
) (any, error) {
	if handle == nil {
		return nil, methodNotImplemented(request.Method)
	}
	params, err := decodeParams[Request](request)
	if err != nil {
		return nil, err
	}
	response, err := handle(ctx, params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

func dispatchNotificationContext[Params any](
	ctx context.Context,
	request *jsonrpc.Request,
	handle func(context.Context, *Params),
) error {
	if handle == nil {
		return methodNotImplemented(request.Method)
	}
	params, err := decodeParams[Params](request)
	if err != nil {
		return err
	}
	handle(ctx, params)
	return nil
}

// The reserved-name check happens before this: a standard method never reaches a
// fallback handler, however it is misspelled, because a fallback that could
// intercept one would be a second path through the capability gate.
func dispatchExtension(
	ctx context.Context,
	request *jsonrpc.Request,
	call func(context.Context, *ExtRequest) (json.RawMessage, error),
	notify func(context.Context, *ExtNotification),
) (any, error) {
	if err := validateExtensionMethod(request.Method); err != nil {
		if !request.IsCall() {
			// Unknown notifications have no response channel. Ignoring a reserved
			// future method also prevents today's fallback from claiming tomorrow's
			// standard notification.
			return nil, nil //nolint:nilnil // an ignored notification has no result or error.
		}
		return nil, newError(ErrorCodeMethodNotFound, "method %q is not an extension method", request.Method)
	}
	if !request.IsCall() {
		if notify == nil {
			return nil, methodNotImplemented(request.Method)
		}
		notify(ctx, &ExtNotification{Method: request.Method, Params: request.Params})
		// A notification has no result and no failure to report. The connection
		// knows not to write a response for one, so both being nil is the answer
		// rather than the absence of one.
		return nil, nil //nolint:nilnil // a notification has neither a result nor an error.
	}
	if call == nil {
		return nil, methodNotImplemented(request.Method)
	}
	result, err := call(ctx, &ExtRequest{Method: request.Method, Params: request.Params})
	if err != nil {
		return nil, err
	}
	if result == nil {
		// An extension may legitimately have nothing to say, and its response type
		// is whatever the two peers agreed on — including an empty object.
		return json.RawMessage("{}"), nil
	}
	return result, nil
}

// A validation failure here is -32602 and not -32603: the peer sent something the
// schema does not permit, and saying so is more useful than "internal error".
func decodeParams[Params any](request *jsonrpc.Request) (*Params, error) {
	params := new(Params)
	if len(request.Params) == 0 {
		// Absent params are valid wherever none are required. A type that does
		// require some fails its own decode below when it is given an object with
		// nothing in it, so this only reaches the zero value where that is legal.
		if err := json.Unmarshal([]byte("{}"), params); err != nil {
			return nil, newError(ErrorCodeInvalidParams, "method %q requires params: %s", request.Method, err)
		}
		return params, nil
	}
	if err := json.Unmarshal(request.Params, params); err != nil {
		return nil, newError(ErrorCodeInvalidParams, "params for method %q are invalid: %s", request.Method, err)
	}
	return params, nil
}

// This runs on the ordered delivery loop, so it deliberately does not go through the
// generated codec: validating a whole payload there would hold up every message
// behind it, and a payload that does not decode fails again in the handler, where
// the failure can be reported.
func decodeInto(params json.RawMessage, into any) error {
	if len(params) == 0 {
		return errNoParams
	}
	return json.Unmarshal(params, into)
}

var errNoParams = errors.New("acp: the request carries no params")

func paramsRequired(operation, fields string) error {
	return fmt.Errorf("acp: %s requires non-nil params with %s", operation, fields)
}

func methodNotImplemented(method string) *Error {
	return newError(ErrorCodeMethodNotFound, "method %q is not implemented", method)
}

func nilHandlerResponse(method string) *Error {
	return newError(ErrorCodeInternalError, "handler for method %q returned a nil response", method)
}
