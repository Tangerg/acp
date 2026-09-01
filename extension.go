package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func validateExtensionMethod(method string) error {
	if isStandardMethod(method) {
		return errReservedMethod(method)
	}
	if !strings.HasPrefix(method, "_") {
		return fmt.Errorf("acp: extension method %q must begin with an underscore; "+
			"the protocol reserves every other name", method)
	}
	return nil
}

func extensionCall(ctx context.Context, l *link, method string, params, result any) error {
	if err := validateExtensionMethod(method); err != nil {
		return err
	}
	return l.call(ctx, method, params, result)
}

func extensionNotify(ctx context.Context, l *link, method string, params any) error {
	if err := validateExtensionMethod(method); err != nil {
		return err
	}
	return l.notify(ctx, method, params)
}
