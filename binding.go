package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// A methodBinding is how one peer serves one standard method. A peer's rows are
// built from its configuration, one for each method it has a handler for, and
// the same set decides the derived advertisement and inbound dispatch: a method
// is advertised and dispatched to exactly when it has a row.
type methodBinding[Conn any] struct {
	// decode reads the params as the method's own type, once, in wire order.
	decode func(json.RawMessage) (any, error)
	// register, when set, records a call in wire order before it is served.
	register func(Conn, *inboundRequest) registration
	// serve answers a request and notify handles a notification; a row sets the
	// one its method's shape needs.
	serve  func(Conn, context.Context, *inboundRequest) (any, error)
	notify func(Conn, context.Context, any)
}

// answering binds a request whose answer serve decides. Every request row is
// built here, so the rule every handler is held to — a response or an error,
// never neither — has one keeper rather than one per method.
func answering[Conn, Request, Response any](
	serve func(Conn, context.Context, *inboundRequest, *Request) (*Response, error),
) *methodBinding[Conn] {
	if serve == nil {
		return nil
	}
	return &methodBinding[Conn]{
		decode: decoderFor[Request],
		serve: func(conn Conn, ctx context.Context, call *inboundRequest) (any, error) {
			response, err := serve(conn, ctx, call, paramsOf[Request](call))
			switch {
			case err != nil:
				return nil, err
			case response == nil:
				return nil, newError(ErrorCodeInternalError, "handler for method %q returned a nil response", call.method)
			default:
				return response, nil
			}
		},
	}
}

// handled binds a request to an application handler given the connection, or
// to nothing when the handler is not set.
func handled[Conn, Request, Response any](
	handler func(context.Context, Conn, *Request) (*Response, error),
) *methodBinding[Conn] {
	if handler == nil {
		return nil
	}
	return answering(func(conn Conn, ctx context.Context, _ *inboundRequest, request *Request) (*Response, error) {
		return handler(ctx, conn, request)
	})
}

// notified binds a notification, which is always served once bound.
func notified[Conn, Notification any](handle func(Conn, context.Context, *Notification)) *methodBinding[Conn] {
	return &methodBinding[Conn]{
		decode: decoderFor[Notification],
		notify: func(conn Conn, ctx context.Context, params any) {
			handle(conn, ctx, decodedAs[Notification]("notification", params))
		},
	}
}

// registeredBy adds the wire-order registration a row's calls need.
func (b *methodBinding[Conn]) registeredBy(register func(Conn, *inboundRequest) registration) *methodBinding[Conn] {
	if b != nil {
		b.register = register
	}
	return b
}

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

// decoderFor is decodeParams as a row's decoder.
func decoderFor[Params any](raw json.RawMessage) (any, error) {
	return decodeParams[Params](raw)
}

// paramsOf returns a call's decoded params.
func paramsOf[Params any](call *inboundRequest) *Params {
	return decodedAs[Params](call.method, call.params)
}

// decodedAs asserts params its row decoded as Params, so a mismatch is a defect
// in the row rather than in a peer.
func decodedAs[Params any](owner string, params any) *Params {
	typed, ok := params.(*Params)
	if !ok {
		panic(fmt.Sprintf("acp: %s params are %T, not %T", owner, params, typed))
	}
	return typed
}

// rawParamsOf returns an extension call's undecoded params.
func rawParamsOf(call *inboundRequest) json.RawMessage {
	params, ok := call.params.(json.RawMessage)
	if !ok {
		panic(fmt.Sprintf("acp: extension %s params are %T", call.method, call.params))
	}
	return params
}

// A sessionScoped request names the session it belongs to, which is what lets
// one row hand a handle to every handler that needs one.
type sessionScoped interface {
	sessionID() SessionID
}

func (x *LoadSessionRequest) sessionID() SessionID            { return x.SessionID }
func (x *ResumeSessionRequest) sessionID() SessionID          { return x.SessionID }
func (x *SetSessionModeRequest) sessionID() SessionID         { return x.SessionID }
func (x *SetSessionConfigOptionRequest) sessionID() SessionID { return x.SessionID }
func (x *DeleteSessionRequest) sessionID() SessionID          { return x.SessionID }
func (x *CloseSessionRequest) sessionID() SessionID           { return x.SessionID }
