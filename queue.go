package acp

import (
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// queue holds the inbound notifications and responses that must be handled in the
// order they arrived.
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
// Inbound requests are deliberately not here. They are independent operations
// served concurrently, because an agent waiting for a permission answer still has
// to be cancellable, and a client asked for permission has to be able to answer
// while updates are still arriving.
//
// The consequence for a handler is worth stating: a notification handler must not
// make a call on the same connection and wait for it, because its own response
// would be queued behind it. Spawn the work instead — which is what the session
// handle is valid beyond the handler call for.
//
// It is unbounded rather than a channel with a size, because a slow handler must
// not stall the read loop: the message that would unblock it may be the next one
// on the wire.
type queue struct {
	mu      sync.Mutex
	pending []jsonrpc.Message
	wake    chan struct{}
}

func newQueue() *queue {
	return &queue{wake: make(chan struct{}, 1)}
}

func (q *queue) push(message jsonrpc.Message) {
	q.mu.Lock()
	q.pending = append(q.pending, message)
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
		// Already awake, or about to be. The slice is what carries the work.
	}
}

func (q *queue) take() []jsonrpc.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.pending
	q.pending = nil
	return batch
}

func (q *queue) awake() <-chan struct{} { return q.wake }
