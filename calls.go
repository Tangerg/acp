package acp

import (
	"sync"
	"sync/atomic"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// calls is the outbound half of a link: the requests this side has sent and has
// not yet been answered.
//
// A call is registered before it is written, because a peer fast enough to answer
// during the write would otherwise be answering something nobody was waiting for.
// It is retired when its caller stops waiting, which is what makes a late answer
// discardable rather than the revival of a call that has already returned.
type calls struct {
	next atomic.Int64

	mu sync.Mutex
	// closed is what makes the retirement of every waiter final. It lives under
	// the same lock as the map so that a call cannot be registered into a map that
	// has already been emptied and will never be drained again.
	closed  bool
	waiting map[jsonrpc.ID]*pendingCall
}

// outboundCall is the handle returned to the goroutine waiting for one call. The
// response is interpreted before completed is signalled, so state derived from a
// response changes in the same ordered delivery step that observed it.
type outboundCall struct {
	id        jsonrpc.ID
	completed <-chan error
}

type pendingCall struct {
	completed chan error
	accept    func(*jsonrpc.Response) error
	abandon   func()
}

func newCalls() *calls {
	return &calls{waiting: make(map[jsonrpc.ID]*pendingCall)}
}

func (c *calls) begin(
	accept func(*jsonrpc.Response) error,
	abandon func(),
) (outboundCall, bool) {
	id := jsonrpc2.Int64ID(c.next.Add(1))
	completed := make(chan error, 1)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return outboundCall{id: id}, false
	}
	c.waiting[id] = &pendingCall{completed: completed, accept: accept, abandon: abandon}
	return outboundCall{id: id, completed: completed}, true
}

// deliver runs on the ordered delivery loop, so accepting a response finishes
// before any later message can observe the state it changes.
func (c *calls) deliver(response *jsonrpc.Response) {
	c.mu.Lock()
	pending, waiting := c.waiting[response.ID]
	delete(c.waiting, response.ID)
	c.mu.Unlock()

	if !waiting {
		// Either a response to a call whose caller has given up, or a peer
		// answering something nobody asked. Both are discarded, and neither
		// revives a retired call.
		return
	}
	var err error
	if pending.accept != nil {
		err = pending.accept(response)
	}
	pending.completed <- err
}

func (c *calls) retire(id jsonrpc.ID) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.mu.Unlock()
}

// Retiring the waiters belongs to the moment the delivery queue has drained
// rather than the moment the read side ended, because the queue being drained is
// exactly what delivers the answers they are waiting for.
func (c *calls) close() {
	c.mu.Lock()
	c.closed = true
	waiting := c.waiting
	c.waiting = make(map[jsonrpc.ID]*pendingCall)
	c.mu.Unlock()

	for _, pending := range waiting {
		if pending.abandon != nil {
			pending.abandon()
		}
	}
}
