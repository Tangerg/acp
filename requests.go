package acp

import (
	"context"
	"fmt"
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
	mu      sync.Mutex
	serving map[jsonrpc.ID]*inboundRequest
}

// inboundRequest keeps the right to answer and the work it controls together.
// Splitting them across maps allowed cancellation to change the context without
// leaving the fact needed to choose the protocol-mandated response.
type inboundRequest struct {
	cancel    context.CancelFunc
	settled   chan struct{}
	answered  bool
	cancelled bool
}

type requestClaim struct {
	cancelled bool
}

func newRequests() *requests {
	return &requests{serving: make(map[jsonrpc.ID]*inboundRequest)}
}

// Both refusals are terminal: an answer written under a reused identifier would
// be ambiguous, and a peer past the in-flight bound is one this side cannot
// follow.
func (r *requests) accept(id jsonrpc.ID, cancel context.CancelFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, occupied := r.serving[id]; occupied {
		cancel()
		return fmt.Errorf("acp: the peer reused active request id %v", id.Raw())
	}
	if len(r.serving) >= maxInflightRequests {
		cancel()
		return fmt.Errorf("%w: %d", errTooManyInflight, maxInflightRequests)
	}
	r.serving[id] = &inboundRequest{cancel: cancel, settled: make(chan struct{})}
	return nil
}

// The last step of serving a request and not the first: the right to answer is
// held on this record, so releasing it before the answer is written would throw
// the answer away.
func (r *requests) release(id jsonrpc.ID) {
	r.mu.Lock()
	request := r.serving[id]
	delete(r.serving, id)
	r.mu.Unlock()

	if request != nil {
		request.cancel()
		close(request.settled)
	}
}

// A claim says who will answer; this says the answer's write has settled, which
// is what cancellation needs to preserve peer-visible ordering.
func (r *requests) settlement(id jsonrpc.ID) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if request := r.serving[id]; request != nil {
		return request.settled
	}
	return nil
}

// cancel marks a $/cancel_request as well as stopping its work. If the handler
// cannot produce a valid response, that fact makes -32800 mandatory instead of
// letting an ordinary Go context error escape as -32603.
func (r *requests) cancel(id jsonrpc.ID) {
	r.mu.Lock()
	request := r.serving[id]
	if request != nil {
		request.cancelled = true
	}
	r.mu.Unlock()

	if request != nil {
		request.cancel()
	}
}

// interrupt is cancellation of work required by a higher-level ACP operation.
// session/cancel still requires a valid PromptResponse, so it must not be
// mistaken for the protocol-level request cancellation above.
func (r *requests) interrupt(id jsonrpc.ID) {
	r.mu.Lock()
	request := r.serving[id]
	r.mu.Unlock()

	if request != nil {
		request.cancel()
	}
}

// The claim carries the wire fact needed to choose the answer, so that no caller
// has to read mutable request state to make that choice.
func (r *requests) claim(id jsonrpc.ID) (requestClaim, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	request := r.serving[id]
	if request == nil || request.answered {
		// Already finished, or never ours.
		return requestClaim{}, false
	}
	request.answered = true
	return requestClaim{cancelled: request.cancelled}, true
}
