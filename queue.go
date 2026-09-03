package acp

import (
	"context"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// queue holds every inbound message until the ordered part of its lifecycle has
// run.
//
// Order is the point, and it is two promises rather than one.
//
// session/update is a stream — message chunks, tool calls, plans, in the order
// the agent produced them — so handling two of them concurrently would deliver a
// turn's output scrambled.
//
// And a response is delivered only after every notification that arrived before
// it. Without that, Prompt could return while the last chunk of the turn it
// describes was still queued, and a caller would see a turn end before hearing how
// it ended. The protocol puts those updates before the response on the wire on
// purpose; this keeps them there.
//
// Calls are only admitted here; their handlers still run concurrently. This
// preserves wire order for registration and handshake publication without
// serializing an agent waiting for a permission answer behind other work.
//
// The consequence for a handler is worth stating: a notification handler must not
// make a call on the same connection and wait for it, because its own response
// would be queued behind it. Spawn the work instead — which is what the session
// handle is valid beyond the handler call for.
//
// A slice rather than a sized channel, because refusing to read until a handler
// finishes would deadlock the one case the handler rule already warns about: the
// message that releases it may be the next one on the wire. The depth is bounded
// instead, and breaching it ends the connection. See limits.go.
type queue struct {
	mu      sync.Mutex
	pending []delivery
	wake    chan struct{}
	limit   int
}

// delivery carries the request context created while the read side was alive.
// A call drained after a read failure must start cancelled rather than inherit
// the uncancelled context used only to drain notifications and responses.
type delivery struct {
	message jsonrpc.Message
	ctx     context.Context //nolint:containedctx // created at read time so drained calls start cancelled.
}

func newQueue(limit int) *queue {
	return &queue{wake: make(chan struct{}, 1), limit: limit}
}

func (q *queue) push(message jsonrpc.Message) bool {
	return q.pushDelivery(delivery{message: message})
}

func (q *queue) pushCall(ctx context.Context, message *jsonrpc.Request) bool {
	return q.pushDelivery(delivery{message: message, ctx: ctx})
}

// A refusal is the connection's to act on: a queue does not know how to end one.
func (q *queue) pushDelivery(pending delivery) bool {
	q.mu.Lock()
	if len(q.pending) >= q.limit {
		q.mu.Unlock()
		return false
	}
	q.pending = append(q.pending, pending)
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
		// Already awake, or about to be. The slice is what carries the work.
	}
	return true
}

func (q *queue) take() []delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.pending
	q.pending = nil
	return batch
}

func (q *queue) awake() <-chan struct{} { return q.wake }
