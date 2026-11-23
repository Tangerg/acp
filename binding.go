package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The serving side of the protocol is two tables, one per peer, mapping each
// standard method this side serves to how it is served. Construction, the
// capability advertisement and inbound dispatch all read the same table, so a
// method served by a handler, advertised by it, and dispatched to it is one row.

// emptyParams stands in for an absent or null params member, both of which the
// schema permits wherever a method requires no property.
var emptyParams = json.RawMessage(`{}`)

// decodeParams decodes a request's params into the method's own type. A failure
// is the peer sending something the schema does not permit.
func decodeParams[Params any](raw json.RawMessage) (*Params, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = emptyParams
	}
	params := new(Params)
	if err := json.Unmarshal(raw, params); err != nil {
		return nil, err
	}
	return params, nil
}

// decoderFor is decodeParams as a binding's decoder.
func decoderFor[Params any](raw json.RawMessage) (any, error) {
	return decodeParams[Params](raw)
}

// paramsOf returns a call's decoded params. The call's binding decoded them as
// Params, so a mismatch is a defect in the binding table rather than in a peer.
func paramsOf[Params any](call *inboundRequest) *Params {
	params, ok := call.params.(*Params)
	if !ok {
		panic(fmt.Sprintf("acp: %s params are %T, not %T", call.method, call.params, params))
	}
	return params
}

// rawParamsOf returns an extension call's undecoded params.
func rawParamsOf(call *inboundRequest) json.RawMessage {
	params, ok := call.params.(json.RawMessage)
	if !ok {
		panic(fmt.Sprintf("acp: extension %s params are %T", call.method, call.params))
	}
	return params
}

// invalidParams answers params that do not decode as the method's type.
func invalidParams(method string, err error) *Error {
	return newError(ErrorCodeInvalidParams, "params for method %q are invalid: %s", method, err)
}

func methodNotImplemented(method string) *Error {
	return newError(ErrorCodeMethodNotFound, "method %q is not implemented", method)
}

// handlerAnswer turns a typed handler result into an answer, refusing the nil
// response a handler is not permitted to return rather than inventing one.
func handlerAnswer[Response any](method string, response *Response, err error) (any, error) {
	switch {
	case err != nil:
		return nil, err
	case response == nil:
		return nil, newError(ErrorCodeInternalError, "handler for method %q returned a nil response", method)
	default:
		return response, nil
	}
}

// A sessionScoped request names the session it belongs to, which is what lets
// one binding hand a handle to every handler that needs one.
type sessionScoped interface {
	sessionID() SessionID
}

func (x *LoadSessionRequest) sessionID() SessionID            { return x.SessionID }
func (x *ResumeSessionRequest) sessionID() SessionID          { return x.SessionID }
func (x *SetSessionModeRequest) sessionID() SessionID         { return x.SessionID }
func (x *SetSessionConfigOptionRequest) sessionID() SessionID { return x.SessionID }
func (x *DeleteSessionRequest) sessionID() SessionID          { return x.SessionID }
func (x *CloseSessionRequest) sessionID() SessionID           { return x.SessionID }
