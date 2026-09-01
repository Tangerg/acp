package acp

// The failure half of the protocol. [Error] itself is generated, because a
// JSON-RPC error object is a wire type; what is written here is what makes it
// usable as a Go error and what keeps its code comparable.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/Tangerg/acp/internal/jsonrpc2"
)

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

// Is matches on the code rather than on identity, because that is the question a
// caller actually asks: "did the agent say I must authenticate first?" is about a
// code, and answering it should not require extracting a value and comparing a
// field. See [ErrAuthRequired].
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
		// An unknown in-range code is valid — the schema's last arm is an
		// unrestricted int32 — so it has to survive being reported.
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

func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// toWire and errorFromWire are the one place the three states of Data survive the
// JSON-RPC adapter. Reading them through [Opt.Get] alone drops an explicit null on
// the way out and invents a present raw "null" on the way in, so an error relayed
// between two peers is not the error it was given.
func (x *Error) toWire() *jsonrpc2.WireError {
	wire := &jsonrpc2.WireError{Code: int64(x.Code), Message: x.Message}
	switch data, present := x.Data.Get(); {
	case present:
		wire.Data = data
	case x.Data.IsNull():
		wire.Data = json.RawMessage("null")
	}
	return wire
}

func errorFromWire(wire *jsonrpc2.WireError) error {
	if wire.Code < -1<<31 || wire.Code > 1<<31-1 {
		return fmt.Errorf("acp: the peer returned JSON-RPC error code %d outside ACP's int32 range", wire.Code)
	}
	failure := &Error{Code: ErrorCode(wire.Code), Message: wire.Message}
	switch {
	case len(wire.Data) == 0:
		// Absent, which is the zero value and needs saying no other way.
	case string(wire.Data) == "null":
		failure.Data = OptNull[json.RawMessage]()
	default:
		failure.Data = OptValue(wire.Data)
	}
	return failure
}
