package acp

import (
	"context"
	"encoding/json"
	"fmt"
)

// An ExtRequest is a request for a method the specification does not define.
//
// The v1 schema defines extension requests, responses and notifications in both
// directions, so without a way to send and receive them an ACP extension could
// not be implemented through this package at all. That is the ceiling this
// avoids.
//
// The params are raw because there is nothing to decode them against: what an
// extension carries is between the two peers that agreed on it.
type ExtRequest struct {
	// Method is the method name, which is outside the set the specification
	// defines. Names beginning with an underscore are reserved for
	// implementation-specific use.
	Method string
	Params json.RawMessage
}

// An ExtNotification is a notification for a method the specification does not
// define. It expects no response, so a handler for one returns nothing.
type ExtNotification struct {
	Method string
	Params json.RawMessage
}

// errReservedMethod refuses a standard method name on the extension path.
//
// An unrestricted method string would be a hole straight through every invariant
// in this package: a caller could pass session/prompt and bypass the generated
// params type, the outbound validation, the session-ID binding and the capability
// gate. A standard method has exactly one path through the typed codec, and this
// is what keeps it the only one.
//
// If a diagnostic tool ever needs raw access to a standard method, that is a
// separately named unsafe API and not a side effect of ordinary extension
// support.
func errReservedMethod(method string) error {
	return fmt.Errorf("acp: %q is a method the specification defines, and the extension API "+
		"does not send it: call the operation for it instead", method)
}

// extensionCall sends an extension request, refusing a reserved name.
func extensionCall(ctx context.Context, c *conn, method string, params, result any) error {
	if isStandardMethod(method) {
		return errReservedMethod(method)
	}
	return c.call(ctx, method, params, result)
}

// extensionNotify sends an extension notification, refusing a reserved name.
func extensionNotify(ctx context.Context, c *conn, method string, params any) error {
	if isStandardMethod(method) {
		return errReservedMethod(method)
	}
	return c.notify(ctx, method, params)
}
