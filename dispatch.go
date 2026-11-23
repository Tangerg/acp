package acp

import (
	"context"
	"fmt"
	"maps"

	"github.com/Tangerg/acp/jsonrpc"
)

// A dispatcher is one peer's inbound half of the protocol: the standard methods
// it serves, and where the extension methods it does not define go. One client
// or agent shares it across its connections; a [dispatch] serves one of them.
type dispatcher[Conn any] struct {
	serving        methodSide
	methods        map[string]*methodBinding[Conn]
	callFallback   func(context.Context, Conn, *ExtRequest) (any, error)
	notifyFallback func(context.Context, Conn, *ExtNotification)
}

// newDispatcher keeps the rows that are bound: a method whose handler is not set
// has no row, so it is neither advertised nor dispatched to.
func newDispatcher[Conn any](
	serving methodSide,
	methods map[string]*methodBinding[Conn],
	callFallback func(context.Context, Conn, *ExtRequest) (any, error),
	notifyFallback func(context.Context, Conn, *ExtNotification),
) *dispatcher[Conn] {
	maps.DeleteFunc(methods, func(_ string, binding *methodBinding[Conn]) bool { return binding == nil })
	return &dispatcher[Conn]{
		serving:        serving,
		methods:        methods,
		callFallback:   callFallback,
		notifyFallback: notifyFallback,
	}
}

// serves reports whether a standard method has a row, which is what a derived
// advertisement and a stated one are both held to.
func (d *dispatcher[Conn]) serves(method string) bool {
	_, bound := d.methods[method]
	return bound
}

// A dispatch is a dispatcher serving one connection, and that connection's link
// side.
type dispatch[Conn any] struct {
	*dispatcher[Conn]
	conn        Conn
	negotiation *negotiation
}

// judge decides in wire order whether this side serves an inbound message, and
// decodes its params once. It returns the method's row, or nil for a method the
// specification does not define, whose params stay raw. Every check happens here, before any goroutine,
// so what a message is judged against is what preceded it on the wire.
func (d *dispatch[Conn]) judge(request *jsonrpc.Request) (*methodBinding[Conn], any, *Error) {
	method := request.Method
	agreed := d.negotiation.agreed()
	// Anything else served before initialize would be served under capabilities
	// nobody has exchanged.
	if agreed == nil && method != methodInitialize {
		return nil, nil, newError(ErrorCodeInvalidRequest, "method %q arrived before initialize", method)
	}
	descriptor, standard := standardMethods[method]
	if !standard {
		return nil, request.Params, nil
	}
	switch {
	case request.IsCall() && descriptor.shape == shapeNotification:
		return nil, nil, newError(ErrorCodeMethodNotFound, "method %q is a notification and has no request form", method)
	case !request.IsCall() && descriptor.shape == shapeRequest:
		return nil, nil, newError(ErrorCodeMethodNotFound, "method %q is a request and arrived without an id", method)
	case descriptor.side != d.serving && descriptor.side != sideProtocol:
		return nil, nil, newError(ErrorCodeMethodNotFound, "method %q is served by the other peer", method)
	}
	if agreed != nil {
		if refusal := agreed.refusesInbound(method); refusal != nil {
			return nil, nil, refusal
		}
	}
	binding, bound := d.methods[method]
	if !bound {
		return nil, nil, methodNotImplemented(method)
	}
	params, err := binding.decode(request.Params)
	if err != nil {
		return nil, nil, newError(ErrorCodeInvalidParams, "params for method %q are invalid: %s", method, err)
	}
	return binding, params, nil
}

func (d *dispatch[Conn]) register(call *inboundRequest, request *jsonrpc.Request) registration {
	binding, params, refusal := d.judge(request)
	switch {
	case refusal != nil:
		return answered(nil, refusal)
	case binding == nil && (d.callFallback == nil || !reservedForExtensions(request.Method)):
		return answered(nil, methodNotImplemented(request.Method))
	}
	call.params = params
	if binding != nil && binding.register != nil {
		return binding.register(d.conn, call)
	}
	return dispatched(callHooks{})
}

// serve runs a dispatched call and holds its response to the agreement, which
// covers responses whose caller has already stopped waiting.
func (d *dispatch[Conn]) serve(ctx context.Context, call *inboundRequest) (any, error) {
	binding, standard := d.methods[call.method]
	if !standard {
		return d.serveExtension(ctx, call)
	}
	response, err := binding.serve(d.conn, ctx, call)
	if err != nil {
		return nil, err
	}
	if checked, ok := response.(outboundChecked); ok {
		if invalid := checked.checkOutbound(d.negotiation.agreed()); invalid != nil {
			return nil, fmt.Errorf("acp: the handler for %q returned a response the negotiated capabilities "+
				"do not permit: %w", call.method, invalid)
		}
	}
	return response, nil
}

// serveExtension hands an extension request to its fallback. An extension may
// have nothing to say, and its response is whatever the peers agreed on,
// including an empty object.
func (d *dispatch[Conn]) serveExtension(ctx context.Context, call *inboundRequest) (any, error) {
	result, err := d.callFallback(ctx, d.conn, &ExtRequest{Method: call.method, Params: rawParamsOf(call)})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return struct{}{}, nil
	}
	return result, nil
}

// serveNotification handles one notification on the ordered delivery loop. One
// this side does not recognise is ignored silently, as the protocol asks, which
// also keeps today's fallback from claiming tomorrow's standard notification. A
// standard one it refuses is returned to be logged.
func (d *dispatch[Conn]) serveNotification(ctx context.Context, request *jsonrpc.Request) error {
	binding, params, refusal := d.judge(request)
	switch {
	case refusal != nil:
		return refusal
	case binding != nil:
		binding.notify(d.conn, ctx, params)
	case d.notifyFallback != nil && reservedForExtensions(request.Method):
		d.notifyFallback(ctx, d.conn, &ExtNotification{Method: request.Method, Params: request.Params})
	}
	return nil
}

func methodNotImplemented(method string) *Error {
	return newError(ErrorCodeMethodNotFound, "method %q is not implemented", method)
}
