package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// requests owns admission, cancellation and the right to answer each inbound
// call. The serving map indexes unanswered obligations; inflight also counts
// handlers still unwinding after their answer was sent by turn cancellation.
type requests struct {
	mu       sync.Mutex
	serving  map[jsonrpc.ID]*inboundRequest
	inflight int
	limit    int
}

// A request record retains its identity after its wire ID becomes reusable.
// Every participant holds this record, so a late handler cannot affect a newer
// request that happens to use the same ID.
type inboundRequest struct {
	owner     *requests
	id        jsonrpc.ID
	ctx       context.Context //nolint:containedctx // this record owns the inbound request's lifetime.
	cancel    context.CancelFunc
	settled   chan struct{}
	sending   chan struct{}
	remaining int
	cancelled bool
}

type requestClaim struct {
	cancelled bool
}

func newRequests(limit int) *requests {
	return &requests{serving: make(map[jsonrpc.ID]*inboundRequest), limit: limit}
}

func (r *requests) accept(ctx context.Context, id jsonrpc.ID) (*inboundRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, occupied := r.serving[id]; occupied {
		return nil, fmt.Errorf("acp: the peer reused active request id %v", id.Raw())
	}
	if r.inflight >= r.limit {
		return nil, fmt.Errorf("%w (limit %d)", errTooManyInflight, r.limit)
	}
	ctx, cancel := context.WithCancel(ctx)
	request := &inboundRequest{
		owner: r, id: id, ctx: ctx, cancel: cancel,
		settled: make(chan struct{}), sending: make(chan struct{}, 1), remaining: 2,
	}
	r.serving[id] = request
	r.inflight++
	return request, nil
}

// Handler work and response delivery can finish independently: turn cancellation
// may answer while a permission handler is still unwinding. Both retain this
// record and count against the bound until they finish.
func (r *inboundRequest) finish() {
	r.cancel()
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.release()
}

// Only the response writer settles the answer. A late handler retains its
// original record, so it cannot answer or remove a later reuse of the wire ID.
func (r *inboundRequest) settle() {
	r.cancel()
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.serving[r.id] == r {
		delete(r.owner.serving, r.id)
	}
	close(r.settled)
	r.release()
}

func (r *inboundRequest) release() {
	r.remaining--
	if r.remaining == 0 {
		r.owner.inflight--
	}
}

// Protocol request cancellation is distinct from interrupting work for an ACP
// turn: only $/cancel_request changes which error the answer must carry.
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

func (r *inboundRequest) claim() (requestClaim, bool) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.serving[r.id] != r {
		return requestClaim{}, false
	}
	// Commit the answer before writing it: the peer can receive the response and
	// reuse its ID before Write returns. The record still retains the write and
	// handler work, but only the serving map owns the right to answer.
	delete(r.owner.serving, r.id)
	return requestClaim{cancelled: r.cancelled}, true
}

// A request-scoped send must commit before its parent's answer, or the peer may
// already have reused the scope's ID. The serving map owns admission; the gate
// lets a claimed answer wait for an admitted write under its response deadline.
func (r *inboundRequest) beginSending(ctx context.Context) error {
	select {
	case r.sending <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.owner.mu.Lock()
	serving := r.owner.serving[r.id] == r
	r.owner.mu.Unlock()
	if !serving {
		r.finishSending()
		return errors.New("acp: the request-scoped elicitation's request has already been answered")
	}
	return nil
}

func (r *inboundRequest) finishSending() { <-r.sending }

// Only the response claimant waits here. Its claim has closed admission, so
// taking the gate proves that every admitted write has finished. The response
// deadline bounds this wait as well as its own transport write.
func (r *inboundRequest) awaitSending(ctx context.Context) error {
	select {
	case r.sending <- struct{}{}:
		r.finishSending()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
