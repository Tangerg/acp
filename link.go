package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// A side is the half of the protocol a link serves.
//
// There is one link type and not two, because the two directions are the same
// message grammar read from opposite ends: an agent answering a prompt is
// simultaneously a caller. What differs between the sides is which handlers they hold and which
// operations they offer, not how a request becomes a response.
type side interface {
	// register runs on the ordered delivery loop, before a call is dispatched, so
	// that a turn and a cancellation cannot race each other. The queue preserves
	// the order the peer sent them; the goroutines that serve them do not, and
	// registering where the ordering is still intact is what makes "the prompt
	// arrived before the cancellation" true of this side's state and not merely of
	// the wire.
	//
	// It must not block: it holds up every message behind it, so an answer it
	// decides on is written on connection-owned work rather than here.
	register(request *jsonrpc.Request, call *inboundRequest) registration

	// serve answers one inbound request or notification. A notification's answer
	// is (nil, nil): there is no response to send, so there is nothing to say.
	serve(ctx context.Context, request *jsonrpc.Request) (result any, err error)
}

// A registration is what a side records about one inbound call before it is
// dispatched, and what it needs to do about that afterwards.
//
// Five things happen to a request at different moments, and putting them all in
// the handler loses the wire-order boundaries between them.
type registration struct {
	// When dispatch is false, result and err are an automatic answer. All answers
	// still go through the request's response claim and settlement.
	dispatch bool
	result   any
	err      error

	// admit lets an ordered claim wait on an earlier request without blocking the
	// delivery loop. Initialize attempts use it because opposite stream directions
	// have no shared ordering point until the earlier response settles.
	admit func(context.Context) error

	// served runs after the side has chosen a result and before the response is
	// written. An agent turn uses this as the fallback commit point for failures
	// rejected above its prompt handler, so a result observed during Write never
	// leaves the session falsely occupied.
	served func()

	// settled runs after the response write has either completed or ended the
	// connection. It releases request-bound records only once their peer-visible
	// obligation is over; identifiers keep an old response from releasing a newer
	// turn for the same session.
	settled func()

	// answered runs once the answer is on the wire, which is the only moment from
	// which this side may safely send: nothing it sends may precede the initialize
	// response that told the peer what the connection can carry.
	answered func()
}

// A link is one peer's end of a JSON-RPC connection: it reads, writes, and holds
// what is outstanding in both directions.
//
// The state is four objects rather than four maps behind one lock, because their
// invariants are unrelated. Outbound calls must not be registered after the last
// delivery; inbound requests must not be forgotten before their answer is
// written; the delivery queue must preserve arrival order; and the work pool must
// not be added to after Wait has begun draining it. One mutex over all four
// would say those four things are one rule.
type link struct {
	transport Connection
	side      side
	logger    *slog.Logger

	// limits is resolved, so every bound read from here is the effective one and
	// no reader has to remember which fields meant "take the default".
	limits Limits

	life     *lifetime
	calls    *calls
	requests *requests
	queue    *queue
}

func newLink(transport Connection, half side, logger *slog.Logger, limits Limits) *link {
	if logger == nil {
		// nil means discard, and never "log somewhere sensible": an agent's stdout
		// is the protocol stream, so a well-meant default logger would corrupt
		// every connection it was supposed to help debug.
		logger = slog.New(slog.DiscardHandler)
	}
	limits = limits.resolve()
	return &link{
		transport: transport,
		side:      half,
		logger:    logger,
		limits:    limits,
		life:      newLifetime(),
		calls:     newCalls(),
		requests:  newRequests(limits.InflightRequests),
		queue:     newQueue(limits.QueuedDeliveries),
	}
}

func (l *link) run() {
	l.life.run(l.readLoop)
	l.life.run(l.deliverLoop)
}

func (l *link) readLoop() {
	// Only the producer can certify that no more messages will enter the queue.
	// Closing the transport may race a successful Read that still needs enqueueing.
	defer close(l.life.readEnded)
	for {
		message, err := l.transport.Read(l.life.ctx)
		if err != nil {
			// A connection owner is already inside endReading when its context is
			// cancelled. Re-entering the same sync.Once from a Read unblocked by
			// that cancellation can make a transport Close that joins its reader
			// wait on itself.
			if l.life.ctx.Err() != nil {
				return
			}
			l.endReading(err)
			return
		}
		switch message := message.(type) {
		case *jsonrpc.Request:
			if message == nil {
				l.endReading(errors.New("acp: transport returned a nil request"))
				return
			}
			if !l.admit(message) {
				return
			}
		case *jsonrpc.Response:
			if message == nil {
				l.endReading(errors.New("acp: transport returned a nil response"))
				return
			}
			if !l.queue.push(message) {
				l.overflowed()
				return
			}
		default:
			l.endReading(fmt.Errorf("acp: transport returned an invalid message of type %T", message))
			return
		}
	}
}

// Every bound a peer can push against is reached from here, because this is the
// only place messages enter. A false answer stops the read loop.
func (l *link) admit(request *jsonrpc.Request) bool {
	// Cancellation is the one message whose effect must overtake queued work;
	// making it wait behind the request it stops defeats it.
	if request.Method == methodCancelRequest && !request.IsCall() {
		l.cancelInflight(request)
		return true
	}
	if !request.IsCall() {
		if shapeErr := requestShapeError(request); shapeErr != nil {
			l.logger.Warn("acp: a request method arrived as a notification and was dropped",
				slog.String("method", request.Method))
			return true
		}
		if !l.queue.push(request) {
			l.overflowed()
			return false
		}
		return true
	}

	// The request travels on its own context, which is also what carries its
	// identity to a handler that needs to name the call it is answering without
	// being handed one. See elicitation.go.
	call, err := l.requests.accept(l.life.ctx, request.ID)
	if err != nil {
		l.endReading(err)
		return false
	}
	call.ctx = withServingRequest(call.ctx, call, request.Method)
	if !l.queue.pushCall(call, request) {
		call.settle()
		call.finish()
		l.overflowed()
		return false
	}
	return true
}

// See limits.go for why a full backlog ends the connection instead of applying
// backpressure.
func (l *link) overflowed() {
	l.endReading(fmt.Errorf("%w (limit %d)", errTooManyQueued, l.limits.QueuedDeliveries))
}

// deliverLoop owns the moment the connection becomes observably over: it is the
// last thing to stop, so nothing it still held is reported as lost.
func (l *link) deliverLoop() {
	defer l.life.finishDelivering(l.calls.close)

	for {
		batch := l.queue.take()
		for _, pending := range batch {
			l.deliver(pending)
		}
		if len(batch) > 0 {
			continue // something may have arrived while that batch ran
		}

		select {
		case <-l.queue.awake():
		case <-l.life.readEnded:
			// Drain what arrived before the read side ended. An agent's final
			// tool-call updates are exactly the messages that arrive last, and
			// dropping them would lose the end of a turn — as would dropping the
			// response that followed them.
			for _, pending := range l.queue.take() {
				l.deliver(pending)
			}
			return
		}
	}
}

func (l *link) deliver(pending delivery) {
	switch message := pending.message.(type) {
	case *jsonrpc.Request:
		l.deliverRequest(pending.serveContext(l.life.ctx), message, pending.request)
	case *jsonrpc.Response:
		l.calls.deliver(message)
	}
}

func (l *link) deliverRequest(ctx context.Context, request *jsonrpc.Request, call *inboundRequest) {
	if !request.IsCall() {
		if _, err := l.side.serve(ctx, request); err != nil {
			l.logger.Error("acp: handling a notification failed",
				slog.String("method", request.Method), slog.Any("error", err))
		}
		return
	}

	entry := registration{err: requestShapeError(request)}
	if entry.err == nil {
		entry = l.side.register(request, call)
	}

	l.life.run(func() {
		defer call.finish()
		result, err := entry.result, entry.err
		if entry.admit != nil {
			err = entry.admit(ctx)
		}
		if entry.dispatch && err == nil {
			result, err = l.side.serve(ctx, request)
		}
		if entry.served != nil {
			entry.served()
		}
		// The answer is written under the connection's context rather than this
		// request's: the request's has just been cancelled in the case that matters
		// most, and a cancelled turn still owes the peer an answer.
		written := l.respond(call, result, err) //nolint:contextcheck // deliberate; see above.
		if entry.settled != nil {
			<-call.settled
			entry.settled()
		}
		if written && entry.answered != nil {
			// Only when the peer has it. An agent whose initialize response never
			// reached the client must not go on to send as though it had.
			entry.answered()
		}
	})
}

// requestShapeError checks the schema's distinction between calls and notifications.
//
// The schema says of every standard method whether it expects a response, and a
// message that contradicts it is not that method. The distinction is not cosmetic:
// terminal/kill sent as a notification would kill a terminal and answer nobody,
// and session/update sent as a call would deliver an update and then be answered
// with an internal error, because a notification handler has no result to return.
//
// An extension method has no shape to contradict. The schema does not define it,
// so its vendor decides what it is, and refusing one here would be this package
// inventing a grammar it does not own.
func requestShapeError(request *jsonrpc.Request) error {
	descriptor, standard := standardMethods[request.Method]
	if !standard || descriptor.shape == shapeEither {
		return nil
	}

	switch {
	case descriptor.shape == shapeNotification && request.IsCall():
		return newError(ErrorCodeInvalidRequest,
			"method %q is a notification and must be sent without an id", request.Method)

	case descriptor.shape == shapeRequest && !request.IsCall():
		return newError(ErrorCodeInvalidRequest,
			"method %q is a request and must be sent with an id", request.Method)

	default:
		return nil
	}
}

// A $/cancel_request for a request that has already been answered is not an
// error: the peer gave up at the same moment the answer was being written, and
// the race is the protocol's rather than anybody's mistake.
func (l *link) cancelInflight(request *jsonrpc.Request) {
	var params cancelRequestNotification
	if err := json.Unmarshal(request.Params, &params); err != nil {
		l.logger.Warn("acp: a $/cancel_request could not be decoded", slog.Any("error", err))
		return
	}
	if id, ok := jsonrpcID(params.RequestID); ok {
		l.requests.cancel(id)
	}
}

func (l *link) endReading(cause error) {
	l.life.endReading(cause, func() error {
		// The lifetime has already cancelled handlers; closing the transport now
		// releases the peer and the read loop. Messages accepted before this point
		// are still drained under a context of their own.
		err := l.transport.Close()
		if err != nil {
			l.logger.Warn("acp: closing the transport failed", slog.Any("error", err))
		}
		return err
	})
}

// It does not wait: messages already accepted are still delivered, and wait is
// what observes that finishing.
func (l *link) close() error {
	l.endReading(nil)
	<-l.life.released // terminal is written before this is closed
	return l.life.terminal
}

// These delegations let the two sides collaborate with a link rather than with
// its parts: a peer's half of the protocol has no business knowing that an answer
// is claimed in one object and a waiter retired in another.
func (l *link) wait() error    { return l.life.wait() }
func (l *link) ended() bool    { return l.life.ended() }
func (l *link) failure() error { return l.life.failure() }

func (l *link) over() <-chan struct{} { return l.life.delivered }

// A cancellation notification has its own budget because the caller's context
// is already done and a degraded peer must not delay that caller's return.
const cancelRequestTimeout = 5 * time.Second

func (l *link) call(ctx context.Context, method string, params, result any) error {
	call, err := l.send(ctx, method, params, nil, nil)
	if err != nil {
		return err
	}
	return l.awaitInto(ctx, call, result)
}

// The caller owns decoding into its result. Delivery may outlive a cancelled
// waiter, and a user-defined decoder may itself call this connection.
func (l *link) awaitInto(ctx context.Context, call outboundCall, result any) error {
	response, err := l.await(ctx, call)
	if err != nil {
		return err
	}
	return decodeResponse(response, result)
}

// Separate from await because a prompt remains a live turn after its original
// caller has stopped waiting for it.
func (l *link) send(
	ctx context.Context,
	method string,
	params any,
	accept func(*jsonrpc.Response) error,
	abandon func(),
) (outboundCall, error) {
	if err := ctx.Err(); err != nil {
		return outboundCall{}, err
	}
	call, open := l.calls.begin(accept, abandon)
	if !open {
		return call, l.life.failure()
	}
	request, err := jsonrpc2.NewCall(call.id, method, params)
	if err != nil {
		l.calls.retire(call.id)
		return call, err
	}
	if writeErr := l.transport.Write(ctx, request); writeErr != nil {
		l.calls.retire(call.id)
		return call, l.writeFailure(ctx, writeErr)
	}
	return call, nil
}

// Connection-owned state follows response order even after its caller stops
// waiting. Caller-owned result objects are instead decoded by awaitInto.
func (l *link) callObserved(
	ctx context.Context,
	method string,
	params any,
	publish func(*jsonrpc.Response) error,
) error {
	call, err := l.send(ctx, method, params, publish, nil)
	if err != nil {
		return err
	}
	_, err = l.await(ctx, call)
	return err
}

func (l *link) await(ctx context.Context, call outboundCall) (*jsonrpc.Response, error) {
	select {
	case result := <-call.completed:
		return result.response, result.err

	case <-ctx.Done():
		// Prefer an answer already delivered because response arrival and context
		// cancellation are routinely ready in the same scheduler turn.
		select {
		case result := <-call.completed:
			return result.response, result.err
		default:
		}
		// Some responses settle connection-owned state that outlives this
		// caller. Keep those calls registered after the caller leaves: the peer
		// may already have received the request, and only its eventual answer can
		// decide whether that state became live.
		if !call.observeLateResponse {
			l.calls.retire(call.id)
		}
		//nolint:contextcheck // the caller is done; cancellation needs its own budget.
		l.cancelRemotely(call.id)
		return nil, ctx.Err()

	case <-l.life.delivered:
		select {
		case result := <-call.completed:
			return result.response, result.err
		default:
		}
		return nil, l.life.failure()
	}
}

func (l *link) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := jsonrpc2.NewNotification(method, params)
	if err != nil {
		return err
	}
	select {
	case <-l.life.delivered:
		return l.life.failure()
	default:
	}
	if writeErr := l.transport.Write(ctx, request); writeErr != nil {
		return l.writeFailure(ctx, writeErr)
	}
	return nil
}

func (l *link) cancelRemotely(id jsonrpc.ID) {
	l.life.spawn(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), cancelRequestTimeout)
		defer cancel()

		params := &cancelRequestNotification{RequestID: requestIDOf(id)}
		if err := l.notify(ctx, methodCancelRequest, params); err != nil &&
			!errors.Is(err, ErrConnectionClosed) {
			l.logger.Warn("acp: telling the peer to cancel a request failed", slog.Any("error", err))
		}
	})
}

// Returning the caller's exact context error is the transport's proof that it
// committed no part of the message. A wrapped context error is deliberately not
// enough: once commitment is uncertain, another write could corrupt a byte stream.
func (l *link) writeFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && err == ctxErr { //nolint:errorlint // exact identity is the no-commit signal.
		return err
	}
	l.endReading(err)
	// Releasing the transport can wake the reader before this write returns.
	// Wait retains that first terminal error, but it must not replace the error
	// belonging to this operation, including the deadline that interrupted it.
	return err
}

// Responses use a connection-owned deadline because nobody is waiting locally
// to bound a peer that stopped reading.
const responseWriteTimeout = 5 * time.Second

func (l *link) respond(call *inboundRequest, result any, handlerErr error) bool {
	write := l.claimResponse(call, result, handlerErr)
	return write != nil && write()
}

func (l *link) claimResponse(call *inboundRequest, result any, handlerErr error) func() bool {
	claim, claimed := call.claim()
	if !claimed {
		return nil
	}
	return func() bool {
		defer call.settle()
		return l.writeResponse(call, claim, result, handlerErr)
	}
}

// The connection context owns response delivery because a cancelled request
// still requires exactly one JSON-RPC response.
func (l *link) writeResponse(call *inboundRequest, claim requestClaim, result any, handlerErr error) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), responseWriteTimeout)
	defer cancel()
	if err := call.awaitSending(ctx); err != nil {
		l.endReading(fmt.Errorf("acp: awaiting request-scoped writes before responding: %w", err))
		return false
	}
	if result == nil && handlerErr == nil {
		handlerErr = newError(ErrorCodeInternalError,
			"request handler returned neither a result nor an error")
	}
	var response *jsonrpc.Response
	if handlerErr == nil {
		var encodeErr error
		response, encodeErr = jsonrpc2.NewResponse(call.id, result, nil)
		if encodeErr != nil {
			l.logger.Error("acp: encoding a result failed", slog.Any("error", encodeErr))
			handlerErr = newError(ErrorCodeInternalError, "response result could not be encoded")
		}
	}
	if handlerErr != nil {
		// Acting on $/cancel_request permits only a valid result or -32800.
		// Encoding is part of producing that result, so its failure must reach
		// this same decision rather than replace it with another error later.
		if claim.cancelled {
			handlerErr = newError(ErrorCodeRequestCancelled, "request cancelled")
		}
		var encodeErr error
		response, encodeErr = jsonrpc2.NewResponse(call.id, nil, l.wireError(handlerErr))
		if encodeErr != nil {
			l.endReading(encodeErr)
			return false
		}
	}

	if writeErr := l.transport.Write(ctx, response); writeErr != nil {
		l.logger.Error("acp: writing a response failed", slog.Any("error", writeErr))
		l.endReading(writeErr)
		return false
	}
	return true
}

// Handler details stay local unless the handler deliberately returned an ACP
// Error, because arbitrary errors routinely contain paths and host identifiers.
func (l *link) wireError(err error) *jsonrpc2.WireError {
	if err == nil {
		return nil
	}
	var chosen *Error
	if errors.As(err, &chosen) {
		return chosen.toWire()
	}
	l.logger.Error("acp: a handler failed", slog.Any("error", err))
	return newError(ErrorCodeInternalError, "%s", ErrorCodeInternalError).toWire()
}
