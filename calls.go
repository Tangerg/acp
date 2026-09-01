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
	waiting map[jsonrpc.ID]chan *jsonrpc.Response
}

func newCalls() *calls {
	return &calls{waiting: make(map[jsonrpc.ID]chan *jsonrpc.Response)}
}

func (c *calls) begin() (jsonrpc.ID, chan *jsonrpc.Response, bool) {
	id := jsonrpc2.Int64ID(c.next.Add(1))
	replies := make(chan *jsonrpc.Response, 1)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return id, nil, false
	}
	c.waiting[id] = replies
	return id, replies, true
}

func (c *calls) deliver(response *jsonrpc.Response) {
	c.mu.Lock()
	replies, waiting := c.waiting[response.ID]
	delete(c.waiting, response.ID)
	c.mu.Unlock()

	if !waiting {
		// Either a response to a call whose caller has given up, or a peer
		// answering something nobody asked. Both are discarded, and neither
		// revives a retired call.
		return
	}
	replies <- response
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
	c.waiting = make(map[jsonrpc.ID]chan *jsonrpc.Response)
	c.mu.Unlock()
}
