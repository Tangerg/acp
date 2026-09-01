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

// cancelRequestTimeout bounds a message this side owes the peer but no caller is
// waiting for: the notification telling it to stop, and a response written after
// its request's context has been cancelled. It exists so that an unresponsive peer
// cannot hold a caller past its own deadline.
const cancelRequestTimeout = 5 * time.Second

// A side is the half of the protocol a link serves.
//
// There is one link type and not two, because the two directions are the same
// message grammar read from opposite ends: 14 of the specification's methods run
// from the agent to the client, so an agent answering a prompt is simultaneously a
// caller. What differs between the sides is which handlers they hold and which
// operations they offer, not how a request becomes a response.
type side interface {
	// register runs on the read loop, before a call is dispatched, so that a turn
	// and a cancellation cannot race each other. The read loop sees messages in
	// the order the peer sent them; the goroutines that serve them do not, and
	// registering where the ordering is still intact is what makes "the prompt
	// arrived before the cancellation" true of this side's state and not merely of
	// the wire.
	//
	// It must not block: it holds up every message behind it, so an answer it
	// decides on is written on connection-owned work rather than here.
	register(request *jsonrpc.Request) registration

	// serve answers one inbound request or notification. A notification's answer
	// is (nil, nil): there is no response to send, so there is nothing to say.
	serve(ctx context.Context, request *jsonrpc.Request) (result any, err error)
}

// A registration is what a side records about one inbound call before it is
// dispatched, and what it needs to do about that afterwards.
//
// Three things happen to a request at three different moments, and putting them
// all in the handler put two of them in the wrong place.
type registration struct {
	// dispatch is false when the side has answered the request itself. A
	// permission request for a turn that is already being cancelled is answered
	// there rather than shown to a user whose answer would then be thrown away.
	dispatch bool

	// finished runs when the handler returns, before its answer goes out.
	//
	// A turn ends here. The client may send the next prompt the moment it reads
	// the answer to this one, so a session released after the write is a session
	// that refuses the prompt that follows it.
	finished func()

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

	life     *lifetime
	calls    *calls
	requests *requests
	queue    *queue
}

func newLink(transport Connection, half side, logger *slog.Logger) *link {
	if logger == nil {
		// nil means discard, and never "log somewhere sensible": an agent's stdout
		// is the protocol stream, so a well-meant default logger would corrupt
		// every connection it was supposed to help debug.
		logger = slog.New(slog.DiscardHandler)
	}
	return &link{
		transport: transport,
		side:      half,
		logger:    logger,
		life:      newLifetime(),
		calls:     newCalls(),
		requests:  newRequests(),
		queue:     newQueue(),
	}
}

func (l *link) run() {
	l.life.run(l.readLoop)
	l.life.run(l.deliverLoop)
}

func (l *link) readLoop() {
	for {
		message, err := l.transport.Read(l.life.ctx)
		if err != nil {
			l.endReading(err)
			return
		}
		switch message := message.(type) {
		case *jsonrpc.Request:
			l.receive(message)
		case *jsonrpc.Response:
			l.queue.push(message)
		default:
			// The message set is closed, so this is unreachable — but a transport
			// is a caller's code, and a nil or novel message should not be a panic
			// in the read loop.
			l.logger.Warn("acp: a transport produced an unknown message type")
		}
	}
}

// deliverLoop owns the moment the connection becomes observably over: it is the
// last thing to stop, so nothing it was still holding can be reported as lost.
func (l *link) deliverLoop() {
	defer l.life.finishDelivering(l.calls.close)

	for {
		batch := l.queue.take()
		for _, message := range batch {
			l.deliver(l.life.ctx, message)
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
			//
			// Under a context of its own, because the connection's has already been
			// cancelled to stop the handlers still running, and these messages were
			// accepted before that happened.
			for _, message := range l.queue.take() {
				l.deliver(context.WithoutCancel(l.life.ctx), message)
			}
			return
		}
	}
}

func (l *link) deliver(ctx context.Context, message jsonrpc.Message) {
	switch message := message.(type) {
	case *jsonrpc.Request:
		if _, err := l.side.serve(ctx, message); err != nil {
			// A notification has no response channel at all, so this is the only
			// place the failure can go.
			l.logger.Error("acp: handling a notification failed",
				slog.String("method", message.Method), slog.Any("error", err))
		}
	case *jsonrpc.Response:
		l.calls.deliver(message)
	}
}

func (l *link) receive(request *jsonrpc.Request) {
	if !l.acceptsShape(request) {
		return
	}

	// $/cancel_request is handled here rather than queued behind the request it
	// cancels. Queueing it would make cancellation arrive after the work it was
	// meant to stop, which is the same as not implementing it.
	if request.Method == methodCancelRequest {
		l.cancelInflight(request)
		return
	}

	if !request.IsCall() {
		l.queue.push(request)
		return
	}

	ctx, cancel := context.WithCancel(l.life.ctx)
	l.requests.accept(request.ID, cancel)
	entry := l.side.register(request)

	if !entry.dispatch {
		// The side answered it itself. Nothing was started, so there is nothing to
		// finish beyond undoing the registration.
		if entry.finished != nil {
			entry.finished()
		}
		l.requests.release(request.ID)
		return
	}

	l.life.run(func() {
		defer l.requests.release(request.ID)
		result, err := l.side.serve(ctx, request)
		if entry.finished != nil {
			entry.finished()
		}
		// The answer is written under the connection's context rather than this
		// request's: the request's has just been cancelled in the case that matters
		// most, and a cancelled turn still owes the peer an answer.
		l.respond(request.ID, result, err) //nolint:contextcheck // deliberate; see above.
		if entry.answered != nil {
			entry.answered()
		}
	})
}

// acceptsShape reports whether an inbound message may be dispatched, and answers
// it when it may not.
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
func (l *link) acceptsShape(request *jsonrpc.Request) bool {
	descriptor, standard := standardMethods[request.Method]
	if !standard || descriptor.shape == shapeEither {
		return true
	}

	switch {
	case descriptor.shape == shapeNotification && request.IsCall():
		// A call can be told. Answering is better than dropping: the peer is
		// waiting, and an identifier nobody ever answers is a leak on its side.
		l.life.spawn(func() {
			l.writeResponse(request.ID, nil, newError(ErrorCodeInvalidRequest,
				"%s is a notification and has no response, so it must be sent without an id",
				request.Method))
		})
		return false

	case descriptor.shape == shapeRequest && !request.IsCall():
		// A notification cannot be told: there is no identifier to answer under.
		// Dropping it is the whole of the remedy, and the log is the only place the
		// fact can go.
		l.logger.Warn("acp: a request method arrived as a notification and was dropped",
			slog.String("method", request.Method))
		return false

	default:
		return true
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

func (l *link) call(ctx context.Context, method string, params, result any) error {
	id, replies, err := l.send(ctx, method, params)
	if err != nil {
		return err
	}
	return l.await(ctx, id, replies, result)
}

// send writes a call and reports the identifier it went out under, together with
// the channel its answer will arrive on.
//
// It is separate from waiting because the two have more than one policy between
// them. Almost every operation wants its caller's context to govern both, which is
// what call does. A turn does not: the caller's patience and the turn's lifetime
// are different facts, and a prompt whose caller has stopped waiting is still a
// turn the agent owes an answer to.
//
// A caller that does not go on to await must retire the identifier itself.
func (l *link) send(ctx context.Context, method string, params any) (jsonrpc.ID, chan *jsonrpc.Response, error) {
	id, replies, open := l.calls.begin()
	if !open {
		return id, nil, l.life.failure()
	}
	request, err := jsonrpc2.NewCall(id, method, params)
	if err != nil {
		l.calls.retire(id)
		return id, nil, err
	}
	if err := l.transport.Write(ctx, request); err != nil {
		l.calls.retire(id)
		return id, nil, l.writeFailure(err)
	}
	return id, replies, nil
}

func (l *link) await(ctx context.Context, id jsonrpc.ID, replies <-chan *jsonrpc.Response, result any) error {
	select {
	case response := <-replies:
		return decodeResponse(response, result)

	case <-ctx.Done():
		// An answer already in hand is an answer, here for the same reason as
		// below. A turn being cancelled cancels this context, and the response that
		// cancellation was sent to produce arrives immediately before it — so both
		// are routinely ready at once, and select would choose at random.
		select {
		case response := <-replies:
			return decodeResponse(response, result)
		default:
		}
		// Otherwise three steps, and all three are load-bearing. Retire the call so
		// a late response is discarded; tell the peer, on a budget of its own and a
		// goroutine of its own so that budget is not added to this caller's
		// latency; and return the exact ctx.Err(), which preserves DeadlineExceeded
		// rather than flattening every timeout into Canceled.
		l.calls.retire(id)
		//nolint:contextcheck // deliberate; the notification has a budget of its own.
		l.cancelRemotely(id)
		return ctx.Err()

	case <-l.life.delivered:
		select {
		case response := <-replies:
			return decodeResponse(response, result)
		default:
		}
		return l.life.failure()
	}
}

func (l *link) notify(ctx context.Context, method string, params any) error {
	request, err := jsonrpc2.NewNotification(method, params)
	if err != nil {
		return err
	}
	select {
	case <-l.life.delivered:
		return l.life.failure()
	default:
	}
	if err := l.transport.Write(ctx, request); err != nil {
		return l.writeFailure(err)
	}
	return nil
}

func (l *link) cancelRemotely(id jsonrpc.ID) {
	l.life.spawn(func() {
		// WithoutCancel because the caller's context is already done, and this
		// notification is precisely the thing that must still be sent.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), cancelRequestTimeout)
		defer cancel()

		params := &cancelRequestNotification{RequestID: requestIDOf(id)}
		if err := l.notify(ctx, methodCancelRequest, params); err != nil &&
			!errors.Is(err, ErrConnectionClosed) {
			l.logger.Warn("acp: telling the peer to cancel a request failed", slog.Any("error", err))
		}
	})
}

func (l *link) respond(id jsonrpc.ID, result any, handlerErr error) {
	if l.requests.claim(id) {
		l.writeResponse(id, result, handlerErr)
	}
}

// writeResponse writes an answer for a request already claimed, under the
// connection's context rather than the request's: the request's has just been
// cancelled in the case that matters most, and a cancelled turn still owes the
// peer an answer.
func (l *link) writeResponse(id jsonrpc.ID, result any, handlerErr error) {
	if result == nil && handlerErr == nil {
		// A request handler that returns nothing is a bug in the dispatch table,
		// not something to send an empty result for: every request in the schema has
		// a response type, however few properties it has.
		handlerErr = newError(ErrorCodeInternalError, "no result and no error")
	}
	response, err := jsonrpc2.NewResponse(id, result, l.wireError(handlerErr))
	if err != nil {
		// The result could not be encoded, which the peer cannot be told in the
		// same breath as being told the result.
		l.logger.Error("acp: encoding a result failed", slog.Any("error", err))
		response, err = jsonrpc2.NewResponse(id, nil,
			l.wireError(newError(ErrorCodeInternalError, "the result could not be encoded")))
		if err != nil {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.life.ctx), cancelRequestTimeout)
	defer cancel()
	if err := l.transport.Write(ctx, response); err != nil {
		// Through the same terminal path as any other failed write. A connection
		// whose output side has failed can otherwise go on accepting requests it can
		// never answer, and nothing would record why.
		l.logger.Error("acp: writing a response failed", slog.Any("error", err))
		_ = l.writeFailure(err)
	}
}

// wireError decides what a peer is told about a failure.
//
// A handler that returns an *Error has chosen the code, the message and the data
// deliberately, and all three are sent. A handler that returns anything else gets
// a stable internal error, and the detail is logged locally: handler errors
// routinely carry paths, hostnames and internal identifiers, and the peer is not
// entitled to them.
func (l *link) wireError(err error) error {
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

// writeFailure translates a transport write failure. A failed write ends the
// logical connection, which is the Connection contract's last clause and the
// reason this does more than return the error.
func (l *link) writeFailure(err error) error {
	switch {
	case errors.Is(err, ErrConnectionClosed):
		return l.life.failure()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller's own context, not the connection's health.
		return err
	default:
		l.endReading(err)
		return err
	}
}

func (l *link) endReading(cause error) {
	l.life.endReading(cause, func() error {
		// The transport first, so that a peer blocked writing to us is released;
		// then the base context, which stops every handler still running. The
		// messages already queued are drained under a context of their own.
		err := l.transport.Close()
		if err != nil {
			l.logger.Warn("acp: closing the transport failed", slog.Any("error", err))
		}
		return err
	})
}

// close ends the connection, and is idempotent. It does not wait: the messages
// already accepted are still delivered, and wait is what observes that finishing.
//
// It reports the connection's terminal error, which is the same value wait
// reports. One policy rather than two.
func (l *link) close() error {
	l.endReading(nil)
	<-l.life.readEnded // terminal is written before this is closed
	return l.life.terminal
}

// These delegations exist so that the two sides collaborate with a link rather
// than with its parts: a peer's half of the protocol has no business knowing that
// an answer is claimed in one object and a waiter retired in another.
func (l *link) wait() error                    { return l.life.wait() }
func (l *link) ended() bool                    { return l.life.ended() }
func (l *link) spawn(fn func()) bool           { return l.life.spawn(fn) }
func (l *link) failure() error                 { return l.life.failure() }
func (l *link) claimAnswer(id jsonrpc.ID) bool { return l.requests.claim(id) }
func (l *link) cancelRequest(id jsonrpc.ID)    { l.requests.cancel(id) }
func (l *link) retireCall(id jsonrpc.ID)       { l.calls.retire(id) }

// over is closed when the connection is observably finished, for callers that
// have their own waiting to do.
func (l *link) over() <-chan struct{} { return l.life.delivered }

// decodeResponse turns a peer's answer into a result or an error.
//
// The envelope is checked before the payload, because a malformed one has no
// honest reading. JSON-RPC 2.0 says a response carries a result or an error and
// exactly one of them: an answer carrying both says two contradictory things, and
// an answer carrying neither used to be read as a zero-valued success for every
// result type in the schema — which is how a peer that answered nothing at all
// could be understood as having answered a session identifier of "".
//
// An empty object is a result. Several of the schema's responses have only
// optional properties, and {} is how a peer says so.
func decodeResponse(response *jsonrpc.Response, result any) error {
	if response.Error != nil {
		if len(response.Result) > 0 {
			return fmt.Errorf("acp: the peer answered with both a result and an error: %w",
				response.Error)
		}
		var wire *jsonrpc2.WireError
		if !errors.As(response.Error, &wire) {
			return response.Error
		}
		return errorFromWire(wire)
	}
	if len(response.Result) == 0 {
		return errors.New("acp: the peer answered with neither a result nor an error")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

// requestIDOf turns a JSON-RPC identifier into the schema's value union, for the
// one payload that carries one.
func requestIDOf(id jsonrpc.ID) requestID {
	switch value := id.Raw().(type) {
	case string:
		arm := requestIDStr(value)
		return &arm
	case int64:
		arm := requestIDNumber(value)
		return &arm
	default:
		return &requestIDNull{}
	}
}

// jsonrpcID is the reverse, and reports whether the identifier names a request
// this side could have issued. The null identifier cannot: nothing is waiting
// under it.
func jsonrpcID(id requestID) (jsonrpc.ID, bool) {
	switch arm := id.(type) {
	case *requestIDStr:
		return jsonrpc2.StringID(string(*arm)), true
	case *requestIDNumber:
		return jsonrpc2.Int64ID(int64(*arm)), true
	default:
		return jsonrpc.ID{}, false
	}
}
