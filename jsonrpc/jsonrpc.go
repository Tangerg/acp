// Package jsonrpc exposes the message types and codec required by custom ACP
// transports. Request lifecycle and method dispatch remain in package acp.
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

	// An ID is a request identifier: a string, an integer, or explicit null. Its
	// zero value represents an absent identifier and therefore a notification.
	ID = jsonrpc2.ID

	// An Error is the structured error a response may carry instead of a result.
	Error = jsonrpc2.WireError
)

// EncodeMessage encodes one JSON-RPC message without transport framing.
func EncodeMessage(message Message) ([]byte, error) {
	return jsonrpc2.EncodeMessage(message)
}

// DecodeMessage decodes one JSON-RPC message and rejects malformed envelopes.
func DecodeMessage(data []byte) (Message, error) {
	return jsonrpc2.DecodeMessage(data)
}
