package acp

import (
	"context"
	"errors"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// requests owns admission, cancellation and the right to answer each inbound
// call. The serving map indexes unanswered obligations; inflight also counts
// handlers still unwinding after turn cancellation answered for them.
type requests struct {
	mu       sync.Mutex
	serving  map[jsonrpc.ID]*inboundRequest
	inflight int
	limit    int
}

// An inboundRequest is one call this side owes an answer. The record keeps its
// identity after its wire ID becomes reusable, so a late handler cannot answer or
// remove a newer request that reuses the ID.
type inboundRequest struct {
	owner  *requests
	id     jsonrpc.ID
	method string
	// params is the request's params, decoded once in wire order.
	params any

	ctx     context.Context //nolint:containedctx // this record owns the inbound request's lifetime.
	cancel  context.CancelFunc
	settled chan struct{}
	sending chan struct{}

	// The record counts against the inflight bound until both its handler and its
	// answer are done, which can happen in either order.
	handlerDone bool
	answerDone  bool
	cancelled   bool
}

type requestClaim struct {
	cancelled bool
}

var errDuplicateRequestID = errors.New("acp: the request reuses the id of a request still being served")

func newRequests(limit int) *requests {
	return &requests{serving: make(map[jsonrpc.ID]*inboundRequest), limit: limit}
}

func (r *requests) accept(ctx context.Context, request *jsonrpc.Request) (*inboundRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, occupied := r.serving[request.ID]; occupied {
		return nil, errDuplicateRequestID
	}
	if r.inflight >= r.limit {
		return nil, limitError(errTooManyInflight, r.limit)
	}
	ctx, cancel := context.WithCancel(ctx)
	call := &inboundRequest{
		owner: r, id: request.ID, method: request.Method, ctx: ctx, cancel: cancel,
		settled: make(chan struct{}), sending: make(chan struct{}, 1),
	}
	r.serving[request.ID] = call
	r.inflight++
	return call, nil
}

// finish marks the handler side done.
func (r *inboundRequest) finish() {
	r.cancel()
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.handlerDone = true
	r.release()
}

// settle marks the answer side done, after the response was written or the
// connection ended.
func (r *inboundRequest) settle() {
	r.cancel()
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.serving[r.id] == r {
		delete(r.owner.serving, r.id)
	}
	close(r.settled)
	r.answerDone = true
	r.release()
}

// release is called under the owner's lock.
func (r *inboundRequest) release() {
	if r.handlerDone && r.answerDone {
		r.owner.inflight--
	}
}

// cancel records a $/cancel_request: only that changes which error the answer
// must carry, as distinct from interrupting work for an ACP turn.
func (r *requests) cancel(id jsonrpc.ID) {
	r.mu.Lock()
	call := r.serving[id]
	if call != nil {
		call.cancelled = true
	}
	r.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

// answerable reports whether this record still owns the right to answer.
func (r *inboundRequest) answerable() bool {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	return r.owner.serving[r.id] == r
}

// claim takes the right to answer before the answer is written: the peer can
// receive the response and reuse its ID before Write returns.
func (r *inboundRequest) claim() (requestClaim, bool) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.serving[r.id] != r {
		return requestClaim{}, false
	}
	delete(r.owner.serving, r.id)
	return requestClaim{cancelled: r.cancelled}, true
}

// beginSending admits one request-scoped write, which must commit before the
// parent's answer or the peer may already have reused the scope's ID.
func (r *inboundRequest) beginSending(ctx context.Context) error {
	select {
	case r.sending <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !r.answerable() {
		r.finishSending()
		return errors.New("acp: the request this elicitation is scoped to has already been answered")
	}
	return nil
}

func (r *inboundRequest) finishSending() { <-r.sending }

// awaitSending is called only by the claimant of the answer. Its claim has closed
// admission, so taking the gate proves every admitted write has finished.
func (r *inboundRequest) awaitSending(ctx context.Context) error {
	select {
	case r.sending <- struct{}{}:
		r.finishSending()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
