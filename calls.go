package acp

import (
	"context"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// calls is the outbound half of a link: the requests this side has sent and that
// have not been answered.
//
// A call is registered before it is written, because a peer fast enough to answer
// during the write would otherwise answer something nobody was waiting for. Most
// calls are retired when their caller stops waiting, so a late answer is
// discarded. A call whose answer settles connection-owned state stays registered
// instead: the peer may already have the request.
type calls struct {
	mu   sync.Mutex
	next int64
	// closed makes retiring every waiter final; it shares the lock with the map
	// so that no call is registered into a map that will never be drained again.
	closed  bool
	waiting map[jsonrpc.ID]*pendingCall
	limit   int
	changed chan struct{}
}

// outboundCall is the handle the goroutine waiting for one call holds.
type outboundCall struct {
	id                  jsonrpc.ID
	completed           <-chan callResult
	observeLateResponse bool
}

type pendingCall struct {
	completed chan callResult
	// accept settles connection-owned state from the response on the delivery
	// loop, before any later message is delivered. abandon runs instead when the
	// connection ends without an answer.
	accept  func(*jsonrpc.Response) error
	abandon func()
}

type callResult struct {
	response *jsonrpc.Response
	err      error
}

func newCalls(limit int) *calls {
	return &calls{waiting: make(map[jsonrpc.ID]*pendingCall), limit: limit}
}

func (c *calls) begin(accept func(*jsonrpc.Response) error, abandon func()) (outboundCall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return outboundCall{}, ErrConnectionClosed
	}
	if len(c.waiting) >= c.limit {
		return outboundCall{}, limitError(errTooManyPendingCalls, c.limit)
	}
	c.next++
	id := jsonrpc.Int64ID(c.next)
	completed := make(chan callResult, 1)
	c.waiting[id] = &pendingCall{completed: completed, accept: accept, abandon: abandon}
	return outboundCall{id: id, completed: completed, observeLateResponse: accept != nil}, nil
}

// take removes the call a response answers. A response nobody is waiting for is
// discarded, and cannot revive a retired call.
func (c *calls) take(id jsonrpc.ID) (*pendingCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending, waiting := c.waiting[id]
	if waiting {
		delete(c.waiting, id)
		c.signalCapacity()
	}
	return pending, waiting
}

// deliver hands a response to its call. It runs on the delivery loop, so
// accepting a response finishes before any later message can observe its state.
func (c *calls) deliver(response *jsonrpc.Response) {
	pending, waiting := c.take(response.ID)
	if !waiting {
		return
	}
	var err error
	if pending.accept != nil {
		err = pending.accept(response)
	}
	pending.completed <- callResult{response: response, err: err}
}

// fail ends a call whose response arrived malformed.
func (c *calls) fail(id jsonrpc.ID, cause error) {
	pending, waiting := c.take(id)
	if !waiting {
		return
	}
	if pending.abandon != nil {
		pending.abandon()
	}
	pending.completed <- callResult{err: cause}
}

func (c *calls) retire(id jsonrpc.ID) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.signalCapacity()
	c.mu.Unlock()
}

// awaitCapacity waits for a free slot. Cleanup may wait; ordinary callers receive
// backpressure instead. The map stays the only count: the channel only announces
// a chance to retry.
func (c *calls) awaitCapacity(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConnectionClosed
	}
	if len(c.waiting) < c.limit {
		c.mu.Unlock()
		return nil
	}
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	changed := c.changed
	c.mu.Unlock()
	select {
	case <-changed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *calls) signalCapacity() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}

// close retires every waiter once the delivery queue has drained, because the
// drain is what delivers the answers they are waiting for.
func (c *calls) close() {
	c.mu.Lock()
	c.closed = true
	waiting := c.waiting
	c.waiting = make(map[jsonrpc.ID]*pendingCall)
	c.signalCapacity()
	c.mu.Unlock()

	for _, pending := range waiting {
		if pending.abandon != nil {
			pending.abandon()
		}
	}
}
