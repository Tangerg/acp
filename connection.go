package acp

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// A connection is what both ends of a link have: the link and the agreement the
// handshake settled. Every standard payload this side sends goes through its
// outbound methods, which hold the payload to that agreement.
type connection struct {
	*link
	negotiation *negotiation
}

// Negotiated reports what initialize settled, as a copy, and the zero value
// before it has.
func (c *connection) Negotiated() Negotiated {
	if agreed := c.negotiation.agreed(); agreed != nil {
		return deepCopy(*agreed)
	}
	return Negotiated{}
}

// Close ends the connection and releases what it owns. It reports a failure to
// release, if this call performed the release, and is idempotent; why the
// connection ended is Wait's to report.
func (c *connection) Close() error { return c.close() }

// Wait blocks until the connection has ended and everything it owns has stopped,
// and reports nil for a local close or a peer that hung up cleanly, and otherwise
// the first read, write or release failure. Every caller sees the same value.
func (c *connection) Wait() error { return c.wait() }

// awaitOpen waits until this side may send, which on an agent is after its
// first initialize answer is on the wire.
func (c *connection) awaitOpen(ctx context.Context, method string) error {
	select {
	case <-c.negotiation.opened:
		return nil
	case <-c.over():
		return c.life.failure()
	case <-ctx.Done():
		return fmt.Errorf("acp: %s waited for initialize: %w", method, ctx.Err())
	}
}

// checkOutbound holds one standard payload to the agreement: the method's
// capability first, then what the payload carries.
func (c *connection) checkOutbound(ctx context.Context, method string, payload any) error {
	if err := c.awaitOpen(ctx, method); err != nil {
		return err
	}
	agreed := c.negotiation.agreed()
	if err := agreed.refusesOutbound(method); err != nil {
		return err
	}
	if checked, ok := payload.(outboundChecked); ok {
		return checked.checkOutbound(agreed)
	}
	return nil
}

// sendStandard is the one path a standard request leaves by.
func (c *connection) sendStandard(
	ctx context.Context,
	method string,
	params any,
	accept func(*jsonrpc.Response) error,
	abandon func(),
) (outboundCall, error) {
	if err := c.checkOutbound(ctx, method, params); err != nil {
		return outboundCall{}, err
	}
	return c.send(ctx, method, params, accept, abandon)
}

func (c *connection) notifyStandard(ctx context.Context, method string, params any) error {
	if err := c.checkOutbound(ctx, method, params); err != nil {
		return err
	}
	return c.notify(ctx, method, params)
}

// callFor sends a standard request and decodes its result into a new Response.
func callFor[Response any](ctx context.Context, c *connection, method string, params any) (*Response, error) {
	call, err := c.sendStandard(ctx, method, params, nil, nil)
	if err != nil {
		return nil, err
	}
	return awaitFor[Response](ctx, c.link, call)
}

// A registry is the live connections of one client or agent. Membership ends
// when a connection is released, whether or not anybody enumerates it.
type registry[Conn comparable] struct {
	mu   sync.Mutex
	open []Conn
}

func (r *registry[Conn]) add(connection Conn, life *lifetime) {
	r.mu.Lock()
	r.open = append(r.open, connection)
	r.mu.Unlock()
	life.onRelease(func() {
		r.mu.Lock()
		r.open = slices.DeleteFunc(r.open, func(open Conn) bool { return open == connection })
		r.mu.Unlock()
	})
}

// all is a snapshot: connections that end while it is iterated still appear.
func (r *registry[Conn]) all() iter.Seq[Conn] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Values(slices.Clone(r.open))
}
