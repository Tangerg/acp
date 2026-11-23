package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/Tangerg/acp/jsonrpc"
)

// A side is the half of the protocol a link serves. There is one link type for
// both peers because the two directions are the same grammar read from opposite
// ends: an agent answering a prompt is simultaneously a caller.
type side interface {
	// register runs on the ordered delivery loop, before a call is dispatched,
	// so that the state it records follows the order the peer sent messages in
	// rather than the order goroutines happen to run. It must not block.
	register(call *inboundRequest, request *jsonrpc.Request) registration

	// serve answers one dispatched call on a goroutine of its own.
	serve(ctx context.Context, call *inboundRequest) (any, error)

	// serveNotification handles one notification on the ordered delivery loop.
	serveNotification(ctx context.Context, request *jsonrpc.Request) error
}

// A registration is what a side decides about one call in wire order: either an
// answer it already knows, or dispatch with lifecycle hooks. Build one with
// [answered] or [dispatched].
type registration struct {
	automatic *automaticAnswer
	hooks     callHooks
}

type automaticAnswer struct {
	result any
	err    error
}

// callHooks are the moments of a dispatched call that state outside the handler
// depends on. Every hook is optional.
type callHooks struct {
	// admit lets a call wait, off the delivery loop, for an earlier one to
	// settle; an error is the call's answer.
	admit func(context.Context) error
	// chosen runs once the answer is decided and before it is written, so a
	// result the peer observes during the write never finds stale state.
	chosen func(result any, err error)
	// finished runs once the answer has settled, with the result that reached
	// the wire, or nil when an error was written or nothing could be.
	finished func(written any)
}

func answered(result any, err error) registration {
	return registration{automatic: &automaticAnswer{result: result, err: err}}
}

// answeredThen is an automatic answer whose settlement still matters.
func answeredThen(result any, finished func(written any)) registration {
	return registration{automatic: &automaticAnswer{result: result}, hooks: callHooks{finished: finished}}
}

func dispatched(hooks callHooks) registration {
	return registration{hooks: hooks}
}

// A link is one peer's end of a JSON-RPC connection. Its four collaborators each
// own one invariant: outbound calls must not be registered after the last
// delivery, inbound requests must not be forgotten before their answer is
// written, delivery must preserve arrival order, and the work pool must not grow
// after Wait has begun draining it.
type link struct {
	transport Connection
	side      side
	logger    *slog.Logger
	// limits is resolved, so every bound read from here is the effective one.
	limits Limits

	life     *lifetime
	calls    *calls
	requests *requests
	queue    *queue
}

func newLink(transport Connection, half side, logger *slog.Logger, limits Limits) *link {
	if logger == nil {
		// nil means discard and never a default: an agent's stdout is the
		// protocol stream, so a well-meant default logger would corrupt it.
		logger = slog.New(slog.DiscardHandler)
	}
	limits = limits.resolve()
	return &link{
		transport: transport,
		side:      half,
		logger:    logger,
		limits:    limits,
		life:      newLifetime(),
		calls:     newCalls(limits.PendingCalls),
		requests:  newRequests(limits.InflightRequests),
		queue:     newQueue(limits.QueuedDeliveries),
	}
}

func (l *link) run() {
	l.life.start(l.readLoop)
	l.life.start(l.deliverLoop)
}

func (l *link) readLoop() {
	// Only the producer can certify that no more messages will enter the queue.
	defer close(l.life.readEnded)
	for {
		message, err := l.transport.Read(l.life.ctx)
		if decodeErr, undecodable := errors.AsType[*jsonrpc.DecodeError](err); undecodable {
			l.undecodable(decodeErr)
			continue
		}
		if err != nil {
			// A Read unblocked by this side closing the transport is the owner
			// already inside endReading, and re-entering it would wait on itself.
			if l.life.ctx.Err() == nil {
				l.endReading(err)
			}
			return
		}
		if !l.admit(message) {
			return
		}
	}
}

// undecodable answers a message that framing delivered but JSON-RPC cannot read.
// A malformed response fails only the call it names; anything else is answered
// with an id-less error, without echoing the message back.
func (l *link) undecodable(decodeErr *jsonrpc.DecodeError) {
	if decodeErr.ResponseID.IsValid() {
		l.enqueue(delivery{message: &malformedResponse{id: decodeErr.ResponseID, err: decodeErr}})
		return
	}
	code := ErrorCodeInvalidRequest
	if decodeErr.Syntax {
		code = ErrorCodeParseError
	}
	l.logger.Warn("an undecodable message was refused", slog.Any("error", decodeErr))
	l.life.start(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), responseWriteTimeout)
		defer cancel()
		l.writeErrorResponse(ctx, jsonrpc.NullID(), &Error{Code: code, Message: code.String()})
	})
}

// A malformedResponse travels the delivery queue so that it fails its call in
// the same order a well-formed answer would have reached it.
type malformedResponse struct {
	id  jsonrpc.ID
	err error
}

// admit is the only place messages enter, so every bound a peer can push against
// is reached from here. A false answer stops the read loop.
func (l *link) admit(message jsonrpc.Message) bool {
	switch message := message.(type) {
	case *jsonrpc.Response:
		if message == nil {
			l.endReading(errors.New("acp: transport returned a nil response"))
			return false
		}
		return l.enqueue(delivery{message: message})
	case *jsonrpc.Request:
		if message == nil {
			l.endReading(errors.New("acp: transport returned a nil request"))
			return false
		}
		return l.admitRequest(message)
	default:
		l.endReading(fmt.Errorf("acp: transport returned an invalid message of type %T", message))
		return false
	}
}

func (l *link) admitRequest(request *jsonrpc.Request) bool {
	// Cancellation is the one message whose effect must overtake queued work;
	// making it wait behind the request it stops would defeat it.
	if request.Method == methodCancelRequest && !request.IsCall() {
		l.cancelInflight(request)
		return true
	}
	if !request.IsCall() {
		if descriptor, standard := standardMethods[request.Method]; standard && descriptor.shape == shapeRequest {
			l.logger.Warn("a request method arrived as a notification and was dropped",
				slog.String("method", request.Method))
			return true
		}
		return l.enqueue(delivery{message: request})
	}

	call, err := l.requests.accept(l.life.ctx, request)
	if errors.Is(err, errDuplicateRequestID) {
		l.life.start(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), responseWriteTimeout)
			defer cancel()
			l.writeErrorResponse(ctx, request.ID, newError(ErrorCodeInvalidRequest, "%s", err))
		})
		return true
	}
	if err != nil {
		l.endReading(err)
		return false
	}
	// The request's own context carries its identity to a handler that names the
	// call it is answering without being handed one. See elicitation.go.
	call.ctx = withServingRequest(call.ctx, call)
	if !l.enqueue(delivery{message: request, call: call}) {
		call.settle()
		call.finish()
		return false
	}
	return true
}

func (l *link) enqueue(pending delivery) bool {
	if l.queue.push(pending) {
		return true
	}
	l.endReading(limitError(errTooManyQueued, l.limits.QueuedDeliveries))
	return false
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
			// Drain what arrived before the read side ended: an agent's final
			// tool-call updates and the response after them arrive last.
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
		if pending.call != nil {
			l.deliverCall(pending.call, message)
			return
		}
		if err := l.side.serveNotification(pending.serveContext(l.life.ctx), message); err != nil {
			l.logger.Error("handling a notification failed",
				slog.String("method", message.Method), slog.Any("error", err))
		}
	case *jsonrpc.Response:
		l.calls.deliver(message)
	case *malformedResponse:
		l.calls.fail(message.id, message.err)
	}
}

func (l *link) deliverCall(call *inboundRequest, request *jsonrpc.Request) {
	entry := l.shapeRegistration(request)
	if entry.automatic == nil {
		entry = l.side.register(call, request)
	}
	l.life.start(func() {
		defer call.finish()
		result, err := l.answer(call, entry)
		if entry.hooks.chosen != nil {
			entry.hooks.chosen(result, err)
		}
		// The answer is written under the connection's context: the request's
		// may have just been cancelled, and a cancelled call still owes an answer.
		written := l.respond(call, result, err)
		if entry.hooks.finished != nil {
			<-call.settled
			entry.hooks.finished(written)
		}
	})
}

// answer decides a dispatched call's answer. A call whose right to answer was
// already taken — a permission request that turn cancellation answered — is not
// handed to a handler that would show the user a question nobody can answer.
func (l *link) answer(call *inboundRequest, entry registration) (any, error) {
	if entry.automatic != nil {
		return entry.automatic.result, entry.automatic.err
	}
	if entry.hooks.admit != nil {
		if err := entry.hooks.admit(call.ctx); err != nil {
			return nil, err
		}
	}
	if !call.answerable() {
		return nil, errAlreadyAnswered
	}
	return l.side.serve(call.ctx, call)
}

// errAlreadyAnswered is the answer of a call whose right to answer was taken
// before its handler could run; nothing is written for it.
var errAlreadyAnswered = errors.New("acp: the request was already answered")

// shapeRegistration refuses a standard notification sent with an id: the method
// exists, but not as a request, which is method-not-found.
func (l *link) shapeRegistration(request *jsonrpc.Request) registration {
	descriptor, standard := standardMethods[request.Method]
	if standard && descriptor.shape == shapeNotification {
		return answered(nil, newError(ErrorCodeMethodNotFound,
			"method %q is a notification and has no request form", request.Method))
	}
	return registration{}
}

// cancelInflight acts on a $/cancel_request. One for a request already answered
// is not an error: the peer gave up as the answer was being written.
func (l *link) cancelInflight(request *jsonrpc.Request) {
	var params cancelRequestNotification
	if err := json.Unmarshal(request.Params, &params); err != nil {
		l.logger.Warn("a $/cancel_request could not be decoded", slog.Any("error", err))
		return
	}
	l.requests.cancel(params.RequestID)
}

func (l *link) endReading(cause error) bool {
	return l.life.endReading(cause, func() error {
		err := l.transport.Close()
		if err != nil {
			l.logger.Warn("closing the transport failed", slog.Any("error", err))
		}
		return err
	})
}

// close ends the connection and reports the error releasing its transport, if
// this call released it. It does not wait: messages already accepted are still
// delivered, and wait observes that finishing.
func (l *link) close() error {
	if !l.endReading(nil) {
		return nil
	}
	<-l.life.released
	return l.life.releaseErr
}

func (l *link) wait() error { return l.life.wait() }

// over is closed once delivery has finished.
func (l *link) over() <-chan struct{} { return l.life.delivered }

// cancelRequestTimeout bounds a $/cancel_request, whose caller has already stopped
// waiting and must not be delayed by a degraded peer.
const cancelRequestTimeout = 5 * time.Second

// responseWriteTimeout bounds every write the connection makes on its own behalf,
// because nobody is waiting locally to bound a peer that stopped reading.
const responseWriteTimeout = 5 * time.Second

// send registers and writes a call. It is separate from await because some calls
// settle connection-owned state after their caller stops waiting.
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
	encoded, err := encodePayload(params)
	if err != nil {
		return outboundCall{}, err
	}
	call, err := l.calls.begin(accept, abandon)
	if errors.Is(err, ErrConnectionClosed) {
		return call, l.life.failure()
	}
	if err != nil {
		return call, err
	}
	request := &jsonrpc.Request{ID: call.id, Method: method, Params: encoded}
	if writeErr := l.transport.Write(ctx, request); writeErr != nil {
		l.calls.retire(call.id)
		return call, l.writeFailure(ctx, writeErr)
	}
	return call, nil
}

// encodePayload encodes params, a result or an error object. nil is an absent
// member. Bytes a peer could not read, including invalid UTF-8 a raw value
// carried, are an encoding failure here rather than a corrupt message later.
func encodePayload(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(encoded) {
		return nil, errors.New("acp: the encoded value is not valid UTF-8")
	}
	return encoded, nil
}

func (l *link) call(ctx context.Context, method string, params, result any) error {
	call, err := l.send(ctx, method, params, nil, nil)
	if err != nil {
		return err
	}
	return l.awaitInto(ctx, call, result)
}

// awaitInto waits for a call and decodes its result into a caller-owned value on
// the caller's goroutine: delivery may outlive a cancelled waiter, and a decoder
// may itself call this connection.
func (l *link) awaitInto(ctx context.Context, call outboundCall, result any) error {
	response, err := l.await(ctx, call)
	if err != nil {
		return err
	}
	return decodeResponse(response, result)
}

// callObserved sends a call whose response settles connection-owned state in
// response order, even after its caller stops waiting.
func (l *link) callObserved(ctx context.Context, method string, params any, publish func(*jsonrpc.Response) error) error {
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
		// Prefer an answer already delivered: arrival and cancellation are
		// routinely ready in the same scheduler turn.
		select {
		case result := <-call.completed:
			return result.response, result.err
		default:
		}
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
	encoded, err := encodePayload(params)
	if err != nil {
		return err
	}
	if l.life.ended() {
		return l.life.failure()
	}
	if writeErr := l.transport.Write(ctx, &jsonrpc.Request{Method: method, Params: encoded}); writeErr != nil {
		return l.writeFailure(ctx, writeErr)
	}
	return nil
}

func (l *link) cancelRemotely(id jsonrpc.ID) {
	l.life.start(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), cancelRequestTimeout)
		defer cancel()
		params := &cancelRequestNotification{RequestID: id}
		if err := l.notify(ctx, methodCancelRequest, params); err != nil && !errors.Is(err, ErrConnectionClosed) {
			l.logger.Warn("telling the peer to cancel a request failed", slog.Any("error", err))
		}
	})
}

// writeFailure keeps the connection only when the transport returned the
// caller's exact context error, its proof that nothing was committed. Any other
// failure leaves commitment uncertain, and another write could corrupt the
// stream. The operation's own error is returned either way.
func (l *link) writeFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && err == ctxErr { //nolint:errorlint // exact identity is the no-commit signal.
		return err
	}
	l.endReading(err)
	return err
}

// respond answers a call and returns the result that reached the wire, or nil.
func (l *link) respond(call *inboundRequest, result any, handlerErr error) any {
	write := l.claimResponse(call, result, handlerErr)
	if write == nil {
		return nil
	}
	return write()
}

// claimResponse takes the right to answer now, on whatever goroutine asks, and
// returns the write for the caller to place.
func (l *link) claimResponse(call *inboundRequest, result any, handlerErr error) func() any {
	claim, claimed := call.claim()
	if !claimed {
		return nil
	}
	return func() any {
		defer call.settle()
		return l.writeResponse(call, claim, result, handlerErr)
	}
}

func (l *link) writeResponse(call *inboundRequest, claim requestClaim, result any, handlerErr error) any {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), responseWriteTimeout)
	defer cancel()
	if err := call.awaitSending(ctx); err != nil {
		l.endReading(fmt.Errorf("acp: awaiting request-scoped writes before responding: %w", err))
		return nil
	}
	if result == nil && handlerErr == nil {
		handlerErr = newError(ErrorCodeInternalError, "request handler returned neither a result nor an error")
	}
	if handlerErr == nil {
		encoded, err := encodePayload(result)
		if err == nil {
			if !l.write(ctx, &jsonrpc.Response{ID: call.id, Result: encoded}) {
				return nil
			}
			return result
		}
		l.logger.Error("encoding a result failed", slog.String("method", call.method), slog.Any("error", err))
		handlerErr = newError(ErrorCodeInternalError, "response result could not be encoded")
	}
	// Acting on $/cancel_request permits only a valid result or -32800.
	if claim.cancelled {
		handlerErr = newError(ErrorCodeRequestCancelled, "request cancelled")
	}
	l.writeErrorResponse(ctx, call.id, l.wireError(call.method, handlerErr))
	return nil
}

func (l *link) writeErrorResponse(ctx context.Context, id jsonrpc.ID, failure *Error) {
	encoded, err := encodePayload(failure)
	if err != nil {
		l.logger.Error("encoding an error response failed", slog.Any("error", err))
		encoded, err = encodePayload(newError(ErrorCodeInternalError, "response error could not be encoded"))
		if err != nil {
			l.endReading(err)
			return
		}
	}
	l.write(ctx, &jsonrpc.Response{ID: id, Error: encoded})
}

func (l *link) write(ctx context.Context, response *jsonrpc.Response) bool {
	if err := l.transport.Write(ctx, response); err != nil {
		l.logger.Error("writing a response failed", slog.String("id", response.ID.String()), slog.Any("error", err))
		l.endReading(err)
		return false
	}
	return true
}

// wireError relays only an error the handler deliberately made an ACP [Error];
// arbitrary errors routinely contain paths and host details, so the peer gets
// -32603 and the detail goes to the log.
func (l *link) wireError(method string, err error) *Error {
	if chosen, ok := errors.AsType[*Error](err); ok && chosen != nil {
		return chosen
	}
	if sentinel, ok := errors.AsType[codeSentinelError](err); ok {
		return &Error{Code: ErrorCode(sentinel), Message: ErrorCode(sentinel).String()}
	}
	l.logger.Error("a handler failed", slog.String("method", method), slog.Any("error", err))
	return &Error{Code: ErrorCodeInternalError, Message: ErrorCodeInternalError.String()}
}

// decodeResponse validates the envelope before the payload so that a
// contradictory or empty response cannot pass as a zero-valued result.
func decodeResponse(response *jsonrpc.Response, result any) error {
	if response.Error != nil && len(response.Result) > 0 {
		return errors.New("acp: the peer answered with both a result and an error")
	}
	if response.Error != nil {
		failure := new(Error)
		if err := json.Unmarshal(response.Error, failure); err != nil {
			return fmt.Errorf("acp: the peer answered with a malformed error: %w", err)
		}
		return failure
	}
	if len(response.Result) == 0 {
		return errors.New("acp: the peer answered with neither a result nor an error")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}
