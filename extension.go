package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// An ExtRequest is a request for a method the specification does not define.
// Its params are raw: what an extension carries is between the two peers that
// agreed on it.
type ExtRequest struct {
	// Method begins with an underscore, the prefix the protocol reserves for
	// implementation-specific methods.
	Method string
	Params json.RawMessage
}

// An ExtNotification is a notification for a method the specification does not
// define.
type ExtNotification struct {
	Method string
	Params json.RawMessage
}

// extensionPrefix is how the protocol reserves a method name for an
// implementation.
const extensionPrefix = "_"

// validateExtensionMethod keeps the typed codec the only path for a standard
// method: an unrestricted name would let a caller send session/prompt past the
// params type, the capability gate and the session binding.
func validateExtensionMethod(method string) error {
	if isStandardMethod(method) {
		return fmt.Errorf("acp: %q is a method the specification defines; call the operation for it instead", method)
	}
	if !strings.HasPrefix(method, extensionPrefix) {
		return fmt.Errorf("acp: extension method %q must begin with %q; the protocol reserves every other name",
			method, extensionPrefix)
	}
	return nil
}

func (c *connection) extensionCall(ctx context.Context, method string, params, result any) error {
	if err := validateExtensionMethod(method); err != nil {
		return err
	}
	if err := c.awaitOpen(ctx, method); err != nil {
		return err
	}
	return c.call(ctx, method, params, result)
}

func (c *connection) extensionNotify(ctx context.Context, method string, params any) error {
	if err := validateExtensionMethod(method); err != nil {
		return err
	}
	if err := c.awaitOpen(ctx, method); err != nil {
		return err
	}
	return c.notify(ctx, method, params)
}

// serveExtensionCall routes an extension request to its fallback. A standard or
// unprefixed name never reaches it, so a fallback cannot become a second path
// through the capability gate.
func serveExtensionCall[Conn any](
	ctx context.Context,
	conn Conn,
	call *inboundRequest,
	fallback func(context.Context, Conn, *ExtRequest) (any, error),
) (any, error) {
	if err := validateExtensionMethod(call.method); err != nil || fallback == nil {
		return nil, newError(ErrorCodeMethodNotFound, "method %q is not implemented", call.method)
	}
	result, err := fallback(ctx, conn, &ExtRequest{Method: call.method, Params: rawParamsOf(call)})
	if err != nil {
		return nil, err
	}
	if result == nil {
		// An extension may have nothing to say, and its response is whatever the
		// peers agreed on, including an empty object.
		return struct{}{}, nil
	}
	return result, nil
}

// serveExtensionNotification routes an extension notification to its fallback.
// An unrecognised notification is ignored, as the protocol asks, which also keeps
// today's fallback from claiming tomorrow's standard notification.
func serveExtensionNotification[Conn any](
	ctx context.Context,
	conn Conn,
	method string,
	params json.RawMessage,
	fallback func(context.Context, Conn, *ExtNotification),
) {
	if validateExtensionMethod(method) != nil || fallback == nil {
		return
	}
	fallback(ctx, conn, &ExtNotification{Method: method, Params: params})
}
