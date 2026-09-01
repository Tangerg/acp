package acp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Tangerg/acp/jsonrpc"
)

// The generic halves of dispatch, so that each method's row in a switch is one
// line and the decode, the nil-handler refusal and the validation are stated once
// rather than once per method.

// A nil handler is method-not-found rather than a panic or a silent success: the
// peer asked for something this side does not implement, and that is exactly what
// -32601 means.
func dispatchCall[Request, Response any](
	ctx context.Context,
	request *jsonrpc.Request,
	handle func(context.Context, *Request) (*Response, error),
) (any, error) {
	if handle == nil {
		return nil, newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
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
		// Every request in the schema has a response type, however few properties
		// it has, and a handler that returns neither a response nor an error has
		// not answered.
		return nil, newError(ErrorCodeInternalError, "the handler for %s returned nothing", request.Method)
	}
	return response, nil
}

// A nil handler is method-not-found. A notification has no response, so nobody
// receives that — the connection logs it, which is the only place it can go.
func dispatchNotificationContext[Params any](
	ctx context.Context,
	request *jsonrpc.Request,
	handle func(context.Context, *Params),
) error {
	if handle == nil {
		return newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
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
	if !request.IsCall() {
		if notify == nil {
			return nil, newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
		}
		notify(ctx, &ExtNotification{Method: request.Method, Params: request.Params})
		// A notification has no result and no failure to report. The connection
		// knows not to write a response for one, so both being nil is the answer
		// rather than the absence of one.
		return nil, nil //nolint:nilnil // a notification has neither a result nor an error.
	}
	if call == nil {
		return nil, newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
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
			return nil, newError(ErrorCodeInvalidParams, "%s requires params: %s", request.Method, err)
		}
		return params, nil
	}
	if err := json.Unmarshal(request.Params, params); err != nil {
		return nil, newError(ErrorCodeInvalidParams, "the params of %s are invalid: %s", request.Method, err)
	}
	return params, nil
}

// This runs on the read loop, so it deliberately does not go through the
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
