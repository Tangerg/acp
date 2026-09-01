package acp

import (
	"errors"
	"fmt"
	"strconv"
)

// Error makes the generated [Error] an error, so that a peer's failure can be
// returned as one and inspected with errors.As.
//
// The type itself is generated from the schema, because it is a wire type: a
// JSON-RPC error object with a code, a message and optional data. What is written
// here is only what makes it usable as a Go error.
func (x *Error) Error() string {
	data, hasData := x.Data.Get()
	switch {
	case x.Message == "" && !hasData:
		return "acp: " + x.Code.String()
	case !hasData:
		return "acp: " + x.Code.String() + ": " + x.Message
	default:
		return "acp: " + x.Code.String() + ": " + x.Message + ": " + string(data)
	}
}

// Is compares codes, which is what makes a sentinel match a peer's error.
//
// Wrapping alone would only have made errors.As work, and errors.As is the wrong
// tool for the question a caller actually asks. "Did the agent say I must
// authenticate first?" is a question about a code, and answering it should not
// require the caller to extract a value and compare a field.
func (x *Error) Is(target error) bool {
	var sentinel codeSentinelError
	if errors.As(target, &sentinel) {
		return x.Code == ErrorCode(sentinel)
	}
	var other *Error
	if errors.As(target, &other) {
		return x.Code == other.Code
	}
	return false
}

// String names a code, so that an error message says what went wrong rather than
// quoting a number at the reader.
//
// The unknown case prints the number, because an unknown in-range code is valid:
// the schema's ninth arm is an unrestricted int32, and a code this package has
// never heard of still has to survive being reported.
func (x ErrorCode) String() string {
	switch x {
	case ErrorCodeParseError:
		return "parse error"
	case ErrorCodeInvalidRequest:
		return "invalid request"
	case ErrorCodeMethodNotFound:
		return "method not found"
	case ErrorCodeInvalidParams:
		return "invalid params"
	case ErrorCodeInternalError:
		return "internal error"
	case ErrorCodeRequestCancelled:
		return "request cancelled"
	case ErrorCodeAuthenticationRequired:
		return "authentication required"
	case ErrorCodeResourceNotFound:
		return "resource not found"
	default:
		return "error " + strconv.FormatInt(int64(x), 10)
	}
}

// A codeSentinelError is a package-level error that matches any peer error
// carrying its code.
//
// It is an unexported type rather than an exported *Error, because a package-level
// var of a pointer type is writable by every importer, and one that mutated it
// would silently change how every other importer's errors.Is behaved. A value of
// an unexported type cannot be reassigned into something that means anything else.
type codeSentinelError ErrorCode

func (c codeSentinelError) Error() string {
	return "acp: " + ErrorCode(c).String()
}

// The sentinels, which exist only where control flow needs one. The [ErrorCode]
// constants plus errors.As already cover ordinary inspection, and a sentinel per
// code would be API surface nobody asked for.
var (
	// ErrAuthRequired is how an agent says "authenticate first". It is control
	// flow rather than failure: answering session/new with -32000 is a documented
	// step in the lifecycle, and the client's next move is to authenticate and
	// retry.
	//
	//	session, result, err := conn.NewSession(ctx, params)
	//	if errors.Is(err, acp.ErrAuthRequired) {
	//		// Expected. Authenticate, then retry.
	//	}
	ErrAuthRequired error = codeSentinelError(ErrorCodeAuthenticationRequired)

	// ErrRequestCancelled matches a peer's -32800.
	//
	// Receiving it does not prove that anybody cancelled anything. The schema says
	// execution may be aborted "either due to a cancellation request from the
	// caller or because of resource constraints or shutdown", so this is the peer
	// reporting that it gave up — a different fact from the local context being
	// done, and kept distinct from it.
	ErrRequestCancelled error = codeSentinelError(ErrorCodeRequestCancelled)
)

// ErrConnectionClosed is returned by an operation on a connection that has ended,
// whether by a local Close or by the transport failing.
//
// It is not a wire error and has no code: no peer sent it, and there was nobody to
// send it to. A caller who needs to know why the connection ended asks Wait.
var ErrConnectionClosed = errors.New("acp: the connection is closed")

// newError builds the error this package sends to a peer.
func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
