package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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
	register(request *jsonrpc.Request) registration

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
	// dispatch is false when the side has answered the request itself. A
	// permission request for a turn that is already being cancelled is answered
	// there rather than shown to a user whose answer would then be thrown away.
	dispatch bool

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
			if !l.admit(message) {
				return
			}
		case *jsonrpc.Response:
			if !l.queue.push(message) {
				l.overflowed()
				return
			}
		default:
			// The message set is closed, so this is unreachable — but a transport
			// is a caller's code, and a nil or novel message should not be a panic
			// in the read loop.
			l.logger.Warn("acp: a transport produced an unknown message type")
		}
	}
}

// admit puts one inbound request where it belongs and reports whether reading may
// continue. Every bound a peer can push against is reached from here, because
// this is the only place messages enter.
func (l *link) admit(request *jsonrpc.Request) bool {
	if !l.acceptsShape(request) {
		return true
	}
	// Cancellation is the one message whose effect must overtake queued work;
	// making it wait behind the request it stops defeats it.
	if request.Method == methodCancelRequest {
		l.cancelInflight(request)
		return true
	}
	if !request.IsCall() {
		if !l.queue.push(request) {
			l.overflowed()
			return false
		}
		return true
	}

	ctx, cancel := context.WithCancel(l.life.ctx)
	if err := l.requests.accept(request.ID, cancel); err != nil {
		l.endReading(err)
		return false
	}
	if !l.queue.pushCall(ctx, request) {
		l.overflowed()
		return false
	}
	return true
}

// overflowed ends a connection whose peer is producing faster than this side
// delivers. The backlog is the only thing that grows here, so there is nothing
// left to try once it is full: see limits.go for why this is not backpressure.
func (l *link) overflowed() {
	l.endReading(fmt.Errorf("%w: more than %d messages are waiting to be delivered",
		errTooManyQueued, maxQueuedDeliveries))
}

// deliverLoop owns the moment the connection becomes observably over: it is the
// last thing to stop, so nothing it was still holding can be reported as lost.
func (l *link) deliverLoop() {
	defer l.life.finishDelivering(l.calls.close)

	for {
		batch := l.queue.take()
		for _, pending := range batch {
			l.deliver(l.life.ctx, pending)
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
			for _, pending := range l.queue.take() {
				l.deliver(context.WithoutCancel(l.life.ctx), pending)
			}
			return
		}
	}
}

func (l *link) deliver(ctx context.Context, pending delivery) {
	switch message := pending.message.(type) {
	case *jsonrpc.Request:
		l.deliverRequest(ctx, pending.ctx, message)
	case *jsonrpc.Response:
		l.calls.deliver(message)
	}
}

func (l *link) deliverRequest(drain, requestCtx context.Context, request *jsonrpc.Request) {
	if !request.IsCall() {
		if _, err := l.side.serve(drain, request); err != nil {
			l.logger.Error("acp: handling a notification failed",
				slog.String("method", request.Method), slog.Any("error", err))
		}
		return
	}

	entry := l.side.register(request)

	if !entry.dispatch {
		// The side answered it itself. Nothing was started, so there is no handler
		// lifecycle to settle.
		l.requests.release(request.ID)
		return
	}

	l.life.run(func() {
		defer l.requests.release(request.ID)
		var result any
		var err error
		if entry.admit != nil {
			err = entry.admit(requestCtx)
		}
		if err == nil {
			result, err = l.side.serve(requestCtx, request)
		}
		if entry.served != nil {
			entry.served()
		}
		// The answer is written under the connection's context rather than this
		// request's: the request's has just been cancelled in the case that matters
		// most, and a cancelled turn still owes the peer an answer.
		written := l.respond(request.ID, result, err) //nolint:contextcheck // deliberate; see above.
		if entry.settled != nil {
			entry.settled()
		}
		if written && entry.answered != nil {
			// Only when the peer has it. An agent whose initialize response never
			// reached the client must not go on to send as though it had.
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
func (l *link) wait() error     { return l.life.wait() }
func (l *link) ended() bool     { return l.life.ended() }
func (l *link) spawn(fn func()) { l.life.spawn(fn) }
func (l *link) failure() error  { return l.life.failure() }
func (l *link) claimAnswer(id jsonrpc.ID) bool {
	_, claimed := l.requests.claim(id)
	return claimed
}
func (l *link) interruptRequest(id jsonrpc.ID) { l.requests.interrupt(id) }

// over is closed when the connection is observably finished, for callers that
// have their own waiting to do.
func (l *link) over() <-chan struct{} { return l.life.delivered }
