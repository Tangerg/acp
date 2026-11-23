package acp

import (
	"context"
	"sync"
)

// queue holds every inbound message until the ordered part of its lifecycle has
// run, which keeps two promises: a turn's session/update stream is delivered in
// the order the agent produced it, and a response is delivered only after every
// notification that preceded it on the wire, so a caller never sees a turn end
// before hearing how it ended. Calls are only admitted here; their handlers still
// run concurrently.
//
// The consequence for a handler: a notification handler must not make a call on
// the same connection and wait for it, because its own response is queued behind
// it.
//
// A bounded slice rather than a sized channel, because refusing to read until a
// handler finishes would deadlock exactly that case; breaching the bound ends the
// connection instead.
type queue struct {
	mu      sync.Mutex
	pending []delivery
	wake    chan struct{}
	limit   int
}

// A delivery is one queued message: a [*jsonrpc.Request], a [*jsonrpc.Response],
// or a [*malformedResponse]. call is the inbound record of a request that expects
// a response, and nil otherwise.
type delivery struct {
	message any
	call    *inboundRequest
}

// serveContext is the context a delivery's handling runs under. A call keeps its
// own request context. A notification drained after the connection began ending
// is still delivered, under a context that is not cancelled with it.
func (d delivery) serveContext(connection context.Context) context.Context {
	if d.call != nil {
		return d.call.ctx
	}
	if connection.Err() != nil {
		return context.WithoutCancel(connection)
	}
	return connection
}

func newQueue(limit int) *queue {
	return &queue{wake: make(chan struct{}, 1), limit: limit}
}

// push queues a message, refusing it when the queue is full. A refusal is the
// connection's to act on.
func (q *queue) push(pending delivery) bool {
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
		// Already awake: the slice carries the work.
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
