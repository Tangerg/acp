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

// reservedForExtensions reports whether a name is one the protocol leaves to
// implementations.
func reservedForExtensions(method string) bool {
	return strings.HasPrefix(method, extensionPrefix)
}

// validateExtensionMethod keeps the typed codec the only path for a standard
// method: an unrestricted name would let a caller send session/prompt past the
// params type, the capability gate and the session binding.
func validateExtensionMethod(method string) error {
	if isStandardMethod(method) {
		return fmt.Errorf("acp: %q is a method the specification defines; call the operation for it instead", method)
	}
	if !reservedForExtensions(method) {
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
