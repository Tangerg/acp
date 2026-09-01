package acp

import (
	"context"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// requests is the inbound half of a link: the calls this side is serving, and who
// holds the right to answer each one.
//
// The right is a claim rather than an ownership, because two things race for it.
// A cancelled turn must answer its pending permission requests with the cancelled
// outcome while the user's handler may still be blocked on a dialog; whoever
// claims first answers, and the loser is dropped rather than sending a second
// response for one request.
type requests struct {
	mu       sync.Mutex
	serving  map[jsonrpc.ID]context.CancelFunc
	answered map[jsonrpc.ID]bool
}

func newRequests() *requests {
	return &requests{
		serving:  make(map[jsonrpc.ID]context.CancelFunc),
		answered: make(map[jsonrpc.ID]bool),
	}
}

// accept puts a request on record, which has to happen before anything asks about
// it: claiming the right to answer a request is only possible once there is a
// request to claim.
func (r *requests) accept(id jsonrpc.ID, cancel context.CancelFunc) {
	r.mu.Lock()
	r.serving[id] = cancel
	r.mu.Unlock()
}

// release forgets a request and stops the work descending from it. It is the last
// step of serving one and not the first: the right to answer is held on this
// record, so releasing it before the answer is written would throw the answer
// away.
func (r *requests) release(id jsonrpc.ID) {
	r.mu.Lock()
	cancel := r.serving[id]
	delete(r.serving, id)
	delete(r.answered, id)
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// cancel stops the work descending from a request without answering it. What the
// handler does about being cancelled is the handler's, and for a turn the protocol
// says what that is: answer with the cancelled stop reason.
func (r *requests) cancel(id jsonrpc.ID) {
	r.mu.Lock()
	cancel := r.serving[id]
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// claim takes the right to answer a request, once.
func (r *requests) claim(id jsonrpc.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answered[id] {
		return false
	}
	if _, serving := r.serving[id]; !serving {
		// Already finished, or never ours.
		return false
	}
	r.answered[id] = true
	return true
}
