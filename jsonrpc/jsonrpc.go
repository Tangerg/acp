// Package jsonrpc exposes the JSON-RPC message type a custom transport carries.
//
// It is public for one reason: a transport has to name the thing it reads and
// writes, and framing bytes means turning that thing into bytes and back. Nothing
// else about JSON-RPC appears in this module's API — not request identifiers, not
// the envelope, not the method strings. Those are plumbing, and a caller who has
// to know them has been handed the plumbing.
//
// The set is therefore as small as a byte-stream transport can be written
// against, and no smaller. Widening a package is a minor release and narrowing
// one is not, so this starts at the minimum: a transport frames and unframes, and
// does not mint request identifiers.
package jsonrpc

import "github.com/Tangerg/acp/internal/jsonrpc2"

type (
	// A Message is a JSON-RPC request or response. It is a closed set: the
	// concrete types are [*Request] and [*Response].
	Message = jsonrpc2.Message

	// A Request is a message sent to a peer. One with an identifier is a call and
	// expects a response; one without is a notification and does not.
	Request = jsonrpc2.Request

	// A Response answers a call, carrying either a result or an error, and the
	// identifier of the call it answers.
	Response = jsonrpc2.Response

	// An ID is a request identifier: a string, an integer, or absent.
	ID = jsonrpc2.ID

	// An Error is the structured error a response may carry instead of a result.
	Error = jsonrpc2.WireError
)

// EncodeMessage turns a message into the bytes that go on the wire.
func EncodeMessage(message Message) ([]byte, error) {
	return jsonrpc2.EncodeMessage(message)
}

// DecodeMessage turns wire bytes into a message, which is a [*Request] or a
// [*Response] according to what the bytes contain.
func DecodeMessage(data []byte) (Message, error) {
	return jsonrpc2.DecodeMessage(data)
}
