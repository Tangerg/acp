package acp

import (
	"errors"
	"fmt"
	"strconv"
)

// [Error] is generated, because a JSON-RPC error object is a wire type; this file
// makes it a Go error with comparable codes.

func (x *Error) Error() string {
	data, hasData := x.Data.Get()
	switch {
	case (x.Message == "" || x.Message == x.Code.String()) && !hasData:
		return "acp: " + x.Code.String()
	case !hasData:
		return "acp: " + x.Code.String() + ": " + x.Message
	default:
		return "acp: " + x.Code.String() + ": " + x.Message + ": " + string(data)
	}
}

// Is matches on the code rather than on identity, because "did the agent say I
// must authenticate first?" is a question about a code.
func (x *Error) Is(target error) bool {
	if x == nil {
		return false
	}
	if sentinel, ok := errors.AsType[codeSentinelError](target); ok {
		return x.Code == ErrorCode(sentinel)
	}
	if other, ok := errors.AsType[*Error](target); ok && other != nil {
		return x.Code == other.Code
	}
	return false
}

// String returns the protocol name of a standard code and a decimal label for
// any other.
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
		// The schema's last arm is an unrestricted int32, so an unknown code is
		// valid and has to survive being reported.
		return "error " + strconv.FormatInt(int64(x), 10)
	}
}

// A codeSentinelError matches any [*Error] with its code, independently of the
// mutable error object a peer sent.
type codeSentinelError ErrorCode

func (c codeSentinelError) Error() string {
	return "acp: " + ErrorCode(c).String()
}

var (
	// ErrAuthRequired is how an agent says "authenticate first": answering
	// session/new with -32000 is a documented step in the lifecycle, and the
	// client's next move is to authenticate and retry.
	ErrAuthRequired error = codeSentinelError(ErrorCodeAuthenticationRequired)

	// ErrRequestCancelled matches a peer's -32800. Receiving it does not prove
	// that anybody cancelled anything: the schema lets a peer abort for resource
	// constraints or shutdown too.
	ErrRequestCancelled error = codeSentinelError(ErrorCodeRequestCancelled)
)

// ErrConnectionClosed is returned by an operation on a connection that has ended.
// When the connection ended because of a failure, the error wraps that cause as
// well; [ClientConn.Wait] reports the cause alone.
var ErrConnectionClosed = errors.New("acp: connection is closed")

// ErrNotAdvertised refuses, before anything is sent, an operation or a value the
// peer did not advertise a capability for. It is local: no peer sent it.
var ErrNotAdvertised = errors.New("acp: not advertised by the peer")

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// notAdvertised wraps [ErrNotAdvertised] with what was refused and the
// capability that would have permitted it.
func notAdvertised(what, capability string) error {
	return fmt.Errorf("%w: %s requires %s", ErrNotAdvertised, what, capability)
}
