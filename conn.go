package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tangerg/acp/internal/jsonrpc2"
	"github.com/Tangerg/acp/jsonrpc"
)

// cancelRequestTimeout bounds the notification that tells a peer to stop working
// on a request whose caller has given up.
//
// It exists so that an unresponsive peer cannot hold the caller past its own
// deadline. The caller has already been given ctx.Err() by the time this is sent;
// this is courtesy to the peer, on a budget.
const cancelRequestTimeout = 5 * time.Second

// A handler serves one inbound request or notification.
//
// Dispatch goes through this one type from the start, on both sides. That is what
// lets middleware be added later without an API break — and what keeps it out
// until something needs it, because an extension point with nothing behind it is
// a speculative abstraction.
//
// A notification's handler returns (nil, nil): there is no response to send, so
// there is nothing to say.
type handler func(ctx context.Context, request *jsonrpc.Request) (result any, err error)

// A registration is what a side records about one inbound call before it is
// dispatched, and what it needs to do about that afterwards.
//
// It exists because three things happen to a request at three different moments,
// and putting them all in the handler put two of them in the wrong place. The
// moments are named here so that each one can be given its reason.
type registration struct {
	// dispatch is false when the side has answered the request itself. A
	// permission request for a turn that is already being cancelled is answered
	// here rather than shown to a user whose answer would then be thrown away.
	dispatch bool

	// finished runs when the handler returns, before its answer goes out.
	//
	// A turn ends here. The client may send the next prompt the moment it reads
	// the answer to this one, so a session released after the write is a session
	// that refuses the prompt that follows it.
	finished func()

	// answered runs once the answer is on the wire.
	//
	// Initialization opens here. An agent may not send on a connection whose peer
	// has not yet been told what it agreed to, and a state set inside the handler
	// is one step too early.
	answered func()
}

// A registrar runs on the read loop, before a call is dispatched.
//
// It exists so that a turn and a cancellation cannot race each other. The read
// loop sees messages in the order the peer sent them; the goroutines that serve
// them do not. Registering a turn where the ordering is still intact is what
// makes "the prompt arrived before the cancellation" true of the connection's
// state and not merely of the wire.
//
// It runs after the request is on record, so that it can claim the right to
// answer it. It must not block: it holds up every message behind it, so an answer
// it decides on is written on connection-owned work rather than here.
type registrar func(request *jsonrpc.Request) registration

// A conn is the machinery both sides share.
//
// There is one of these and not two, because the two directions are the same
// message grammar read from opposite ends: 14 of the specification's methods run
// from the agent to the client, so an agent answering a prompt is simultaneously a
// caller. What differs between the sides is which handlers they hold and which
// operations they offer, not how a request becomes a response.
type conn struct {
	transport Connection
	handler   handler
	// register runs on the read loop before dispatch. nil when the side has
	// nothing to register.
	register registrar
	logger   *slog.Logger

	// base scopes everything the connection owns: the read loop, and every
	// inbound request's context. Cancelling it is how Close stops the work
	// descending from a connection that has ended.
	//
	// A context in a struct is ordinarily a smell, because it hides which call a
	// deadline belongs to. This one is not a call's: it is the connection's
	// lifetime, which outlives every context passed to it — a caller who gave
	// Connect a five-second handshake timeout has not asked for the connection to
	// die after five seconds. There is nowhere else for it to live.
	base       context.Context //nolint:containedctx // the connection's lifetime, not a call's.
	cancelBase context.CancelFunc

	nextID atomic.Int64

	mu sync.Mutex
	// pending is the outbound calls waiting for a response. A call removes its own
	// entry when its context finishes, which is what makes a late response
	// discardable rather than a revival of a retired call.
	pending map[jsonrpc.ID]chan *jsonrpc.Response
	// inflight is the inbound requests being served, so that a cancellation can
	// find one.
	inflight map[jsonrpc.ID]context.CancelFunc
	// answered is the inbound requests whose response has been written.
	//
	// It is what makes a race between a handler and a cancellation decidable. A
	// cancelled turn must answer its pending permission requests with the
	// cancelled outcome while the user's handler may still be blocked on a dialog;
	// whoever claims a request first answers it, and the loser is dropped rather
	// than sending a second response for one request.
	answered map[jsonrpc.ID]bool

	closeOnce sync.Once
	// readEnded is closed when the read side has finished: the transport failed,
	// the peer hung up, or this side closed. done is closed later, when everything
	// the read side had already accepted has been delivered.
	//
	// They are two facts and were one. A response read from the transport and
	// queued for delivery is a response this connection accepted, and a call
	// waiting for it must not be told the connection ended instead — which is what
	// happened when EOF arrived immediately behind it and both channels were ready
	// at once.
	readEnded chan struct{}
	done      chan struct{}
	// stopped is set under mu once no further work may be started, so that a
	// cancellation notification is never spawned into a WaitGroup that Wait has
	// already begun draining.
	stopped bool
	// terminal is written once, before done is closed, and read only after — so
	// every caller of wait sees the same value without holding a lock.
	terminal error

	// ordered is the queue of inbound notifications and responses, served in
	// order by one goroutine.
	//
	// Order is the point, and it is two promises rather than one.
	//
	// session/update is a stream — message chunks, tool calls, plans, in the order
	// the agent produced them — so handling two of them concurrently would deliver
	// a turn's output scrambled.
	//
	// And a response is delivered only after every notification that arrived
	// before it. Without that, Prompt could return while the last chunk of the
	// turn it describes was still queued, and a caller would see a turn end before
	// hearing how it ended. The protocol puts those updates before the response on
	// the wire on purpose; this keeps them there.
	//
	// Inbound *requests* are not in this queue. They are independent operations
	// served concurrently, because an agent waiting for a permission answer still
	// has to be cancellable, and because a client asked for permission must be able
	// to answer while updates are still arriving.
	//
	// The consequence for a handler is worth stating: a notification handler must
	// not make a call on the same connection and wait for it, because its own
	// response would be queued behind it. Spawn the work instead — which is what
	// the session handle is valid beyond the handler call for.
	//
	// The queue is unbounded rather than a channel with a size. A slow handler must
	// not stall the read loop, because the request that would unblock it may be
	// the next message on the wire.
	orderedMu    sync.Mutex
	orderedQueue []jsonrpc.Message
	orderedWake  chan struct{}

	// work counts the goroutines serving inbound messages, so that wait can
	// report a connection that has genuinely finished rather than one whose
	// handlers are still running.
	work sync.WaitGroup
}

func newConn(transport Connection, serve handler, register registrar, logger *slog.Logger) *conn {
	if logger == nil {
		// nil means discard, and never "log somewhere sensible": an agent's stdout
		// is the protocol stream, so a well-meant default logger would corrupt
		// every connection it was supposed to help debug.
		logger = slog.New(slog.DiscardHandler)
	}
	base, cancelBase := context.WithCancel(context.Background())
	return &conn{
		transport:   transport,
		handler:     serve,
		register:    register,
		logger:      logger,
		base:        base,
		cancelBase:  cancelBase,
		pending:     make(map[jsonrpc.ID]chan *jsonrpc.Response),
		inflight:    make(map[jsonrpc.ID]context.CancelFunc),
		answered:    make(map[jsonrpc.ID]bool),
		orderedWake: make(chan struct{}, 1),
		readEnded:   make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// run starts the read loop. It returns immediately; the loop ends when the
// transport fails, the peer hangs up, or the connection is closed.
func (c *conn) run() {
	c.work.Go(c.readLoop)
	c.work.Go(c.orderedLoop)
}

// orderedLoop serves queued notifications and responses in arrival order, and
// owns the moment the connection becomes observably over.
//
// It closes done only after draining what the read side had already accepted.
// Anything else would let a call be told the connection ended while its answer
// was still in this queue.
func (c *conn) orderedLoop() {
	defer c.finishDraining()

	for {
		c.orderedMu.Lock()
		batch := c.orderedQueue
		c.orderedQueue = nil
		c.orderedMu.Unlock()

		for _, message := range batch {
			c.serveOrdered(c.base, message)
		}
		if len(batch) > 0 {
			continue // something may have arrived while that batch ran
		}

		select {
		case <-c.orderedWake:
		case <-c.readEnded:
			// Drain what arrived before the read side ended. An agent's final
			// tool-call updates are exactly the messages that arrive last, and
			// dropping them would lose the end of a turn — as would dropping the
			// response that followed them.
			//
			// The context is uncancelled: the connection's own has already been
			// cancelled to stop the handlers still running, and these messages
			// were accepted before that happened.
			c.orderedMu.Lock()
			remaining := c.orderedQueue
			c.orderedQueue = nil
			c.orderedMu.Unlock()
			for _, message := range remaining {
				c.serveOrdered(context.WithoutCancel(c.base), message)
			}
			return
		}
	}
}

// finishDraining marks the connection over, which is the last thing that happens.
//
// The pending calls are dropped here and not when the read side ended, because
// the queue being drained is exactly what delivers the answers they are waiting
// for. Dropping them earlier discarded a response this connection had already
// accepted — and dropping them at all is what makes a later one undeliverable
// rather than a revival of a retired call.
func (c *conn) finishDraining() {
	c.mu.Lock()
	c.stopped = true
	c.pending = make(map[jsonrpc.ID]chan *jsonrpc.Response)
	c.mu.Unlock()
	close(c.done)
}

func (c *conn) serveOrdered(ctx context.Context, message jsonrpc.Message) {
	switch message := message.(type) {
	case *jsonrpc.Request:
		if _, err := c.handler(ctx, message); err != nil {
			// A notification has no response channel at all, so this is the only
			// place the failure can go.
			c.logger.Error("acp: handling a notification failed",
				slog.String("method", message.Method), slog.Any("error", err))
		}
	case *jsonrpc.Response:
		c.deliver(message)
	}
}

// enqueue adds a message to the ordered queue and wakes its server.
func (c *conn) enqueue(message jsonrpc.Message) {
	c.orderedMu.Lock()
	c.orderedQueue = append(c.orderedQueue, message)
	c.orderedMu.Unlock()
	select {
	case c.orderedWake <- struct{}{}:
	default:
		// Already awake, or about to be. The queue is what carries the work.
	}
}

func (c *conn) readLoop() {
	for {
		message, err := c.transport.Read(c.base)
		if err != nil {
			c.endReading(err)
			return
		}
		switch message := message.(type) {
		case *jsonrpc.Request:
			c.receive(message)
		case *jsonrpc.Response:
			c.enqueue(message)
		default:
			// The message set is closed, so this is unreachable — but a transport
			// is a caller's code, and a nil or novel message should not be a panic
			// in the read loop.
			c.logger.Warn("acp: a transport produced an unknown message type")
		}
	}
}

// receive dispatches one inbound message.
func (c *conn) receive(request *jsonrpc.Request) {
	if !c.acceptsShape(request) {
		return
	}

	// $/cancel_request is handled in the read loop rather than queued behind the
	// request it cancels. Queueing it would make cancellation arrive after the
	// work it was meant to stop, which is the same as not implementing it.
	if request.Method == methodCancelRequest {
		c.cancelInflight(request)
		return
	}

	if !request.IsCall() {
		c.enqueue(request)
		return
	}

	// The request goes on record before anything is asked about it. Registration
	// may answer it, and claiming the right to answer a request is only possible
	// once there is a request to claim.
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.inflight[request.ID] = cancel
	c.mu.Unlock()

	// Registration happens here, on the read loop, so that what a later message
	// finds is what the peer's ordering implies.
	entry := registration{dispatch: true}
	if c.register != nil {
		entry = c.register(request)
	}

	// Whatever happens next, the request stops being one this connection is
	// serving. That is the last step rather than the first, because the right to
	// answer a request is held on this record: releasing it before the answer is
	// written would throw the answer away.
	release := func() {
		c.mu.Lock()
		delete(c.inflight, request.ID)
		delete(c.answered, request.ID)
		c.mu.Unlock()
		cancel()
	}
	finished := func() {
		if entry.finished != nil {
			entry.finished()
		}
	}

	if !entry.dispatch {
		// The side answered it itself. Nothing was started, so there is nothing to
		// finish beyond undoing the registration.
		finished()
		release()
		return
	}

	c.work.Go(func() {
		defer release()
		result, err := c.handler(ctx, request)
		// Before the answer goes out: see registration.finished.
		finished()
		// The answer is written under the connection's context rather than this
		// request's: the request's has just been cancelled in the case that
		// matters most, and a cancelled turn still owes the peer an answer.
		c.respond(request.ID, result, err) //nolint:contextcheck // deliberate; see above.
		if entry.answered != nil {
			entry.answered()
		}
	})
}

// acceptsShape reports whether an inbound message may be dispatched, and answers
// it when it may not.
//
// The schema says of every standard method whether it expects a response, and a
// message that contradicts it is not that method. The distinction is not
// cosmetic: terminal/kill sent as a notification would kill a terminal and answer
// nobody, and session/update sent as a call would deliver an update and then be
// answered with an internal error, because a notification handler has no result
// to return. Neither should reach a handler at all.
//
// An extension method has no shape to contradict. The schema does not define it,
// so its vendor decides what it is, and refusing one here would be this package
// inventing a grammar it does not own.
func (c *conn) acceptsShape(request *jsonrpc.Request) bool {
	descriptor, standard := standardMethods[request.Method]
	if !standard || descriptor.shape == shapeEither {
		return true
	}

	if descriptor.shape == shapeNotification && request.IsCall() {
		// A call can be told. Answering is better than dropping: the peer is
		// waiting, and an identifier nobody ever answers is a leak on its side.
		c.spawn(func() {
			c.writeResponse(request.ID, nil, newError(ErrorCodeInvalidRequest,
				"%s is a notification and has no response, so it must be sent without an id",
				request.Method))
		})
		return false
	}
	if descriptor.shape == shapeRequest && !request.IsCall() {
		// A notification cannot be told: there is no identifier to answer under.
		// Dropping it is the whole of the remedy, and the log is the only place
		// the fact can go.
		c.logger.Warn("acp: a request method arrived as a notification and was dropped",
			slog.String("method", request.Method))
		return false
	}
	return true
}

// cancelInflight cancels the request a $/cancel_request names.
//
// A notification for a request that has already been answered is not an error:
// the peer gave up at the same moment the answer was being written, and the race
// is the protocol's rather than anybody's mistake.
func (c *conn) cancelInflight(request *jsonrpc.Request) {
	var params cancelRequestNotification
	if err := json.Unmarshal(request.Params, &params); err != nil {
		c.logger.Warn("acp: a $/cancel_request could not be decoded", slog.Any("error", err))
		return
	}
	id, ok := jsonrpcID(params.RequestID)
	if !ok {
		return
	}
	c.cancelRequest(id)
}

// cancelRequest cancels the context of an inbound request being served. It does
// not answer it: what the handler does about being cancelled is the handler's,
// and for a turn the protocol says what that is — answer with the cancelled stop
// reason.
func (c *conn) cancelRequest(id jsonrpc.ID) {
	c.mu.Lock()
	cancel := c.inflight[id]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// claimResponse takes the right to answer an inbound request, once.
//
// Whoever claims it writes the response; everybody else is dropped. This is what
// "resolved exactly once" is made of, and claiming before doing anything else is
// what makes the race decidable rather than a matter of which goroutine ran first.
func (c *conn) claimResponse(id jsonrpc.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answered[id] {
		return false
	}
	if _, serving := c.inflight[id]; !serving {
		// Already finished, or never ours.
		return false
	}
	c.answered[id] = true
	return true
}

// deliver hands a response to the call waiting for it.
func (c *conn) deliver(response *jsonrpc.Response) {
	c.mu.Lock()
	replies, waiting := c.pending[response.ID]
	delete(c.pending, response.ID)
	c.mu.Unlock()

	if !waiting {
		// Either a response to a call whose caller has given up, or a peer
		// answering something nobody asked. Both are discarded, and neither
		// revives a retired call.
		return
	}
	replies <- response
}

// call sends a request and waits for its response.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id, replies, err := c.send(ctx, method, params)
	if err != nil {
		return err
	}
	return c.await(ctx, id, replies, result)
}

// send writes a call and reports the identifier it went out under, together with
// the channel its answer will arrive on.
//
// It is separate from waiting because the two have more than one policy between
// them. Almost every operation wants its caller's context to govern both, which
// is what call does. A turn does not: the caller's patience and the turn's
// lifetime are different facts, and a prompt whose caller has stopped waiting is
// still a turn the agent owes an answer to.
//
// A caller that does not go on to await must retire the identifier itself.
func (c *conn) send(ctx context.Context, method string, params any) (jsonrpc.ID, chan *jsonrpc.Response, error) {
	id := jsonrpc2.Int64ID(c.nextID.Add(1))
	request, err := jsonrpc2.NewCall(id, method, params)
	if err != nil {
		return id, nil, err
	}

	replies := make(chan *jsonrpc.Response, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return id, nil, c.terminalError()
	default:
	}
	c.pending[id] = replies
	c.mu.Unlock()

	if err := c.transport.Write(ctx, request); err != nil {
		c.retire(id)
		return id, nil, c.writeFailure(err)
	}
	return id, replies, nil
}

// await waits for one call's answer.
func (c *conn) await(ctx context.Context, id jsonrpc.ID, replies <-chan *jsonrpc.Response, result any) error {
	select {
	case response := <-replies:
		return decodeResponse(response, result)

	case <-ctx.Done():
		// An answer already in hand is an answer, here for the same reason as
		// below. A turn being cancelled cancels this context, and the response
		// that cancellation was sent to produce arrives immediately before it —
		// so both are routinely ready at once, and select would choose between
		// them at random.
		select {
		case response := <-replies:
			return decodeResponse(response, result)
		default:
		}
		// Otherwise three steps, and all three are load-bearing. Retire the call
		// so a late response is discarded; tell the peer, on a budget of its own
		// and on a goroutine of its own so that budget is not added to this
		// caller's latency; and return the exact ctx.Err(), which preserves
		// DeadlineExceeded rather than flattening every timeout into Canceled.
		c.retire(id)
		//nolint:contextcheck // deliberate; the notification has a budget of its own.
		c.cancelRemotely(id)
		return ctx.Err()

	case <-c.done:
		// done and a delivered response can be ready at once, and select would
		// pick between them at random. An answer already in hand is an answer:
		// the connection accepted it before it ended, and reporting that it ended
		// instead would lose a completed call.
		select {
		case response := <-replies:
			return decodeResponse(response, result)
		default:
		}
		return c.terminalError()
	}
}

// cancelRemotely tells the peer to stop, without making the caller wait for it.
//
// The notification has an independent budget so that an unresponsive peer cannot
// hold this connection open, and it runs on connection-owned work so that its
// exit is something wait observes rather than a goroutine nobody tracks.
func (c *conn) cancelRemotely(id jsonrpc.ID) {
	c.spawn(func() { c.cancelRemote(id) })
}

// spawn runs work the connection owns, and reports whether it started.
//
// The check and the counter increment are under one lock so that nothing is ever
// added to a WaitGroup that wait has already begun draining. It reports false
// when the connection is already over, because a caller that was relying on the
// work to finish something has to finish it another way.
func (c *conn) spawn(fn func()) bool {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return false
	}
	c.work.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.work.Done()
		fn()
	}()
	return true
}

// notify sends a request that expects no response.
func (c *conn) notify(ctx context.Context, method string, params any) error {
	request, err := jsonrpc2.NewNotification(method, params)
	if err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.terminalError()
	default:
	}
	if err := c.transport.Write(ctx, request); err != nil {
		return c.writeFailure(err)
	}
	return nil
}

func (c *conn) retire(id jsonrpc.ID) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// cancelRemote tells the peer to stop working on a request this side has given up
// on, without letting the peer delay the caller any further.
func (c *conn) cancelRemote(id jsonrpc.ID) {
	// WithoutCancel because the caller's context is already done, and this
	// notification is precisely the thing that must still be sent.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.base), cancelRequestTimeout)
	defer cancel()

	params := &cancelRequestNotification{RequestID: requestIDOf(id)}
	if err := c.notify(ctx, methodCancelRequest, params); err != nil && !errors.Is(err, ErrConnectionClosed) {
		c.logger.Warn("acp: telling the peer to cancel a request failed", slog.Any("error", err))
	}
}

// respond writes a handler's answer, unless something else has already answered.
func (c *conn) respond(id jsonrpc.ID, result any, handlerErr error) {
	if write := c.answer(id, result, handlerErr); write != nil {
		write()
	}
}

// answer claims the right to answer a request and returns the write that does it,
// or nil if something else has already answered.
//
// The two halves are separate because the claim decides a race and the write
// talks to a transport. The read loop needs the first and must not do the second:
// a permission request refused during a cancellation is claimed where the
// ordering is intact and written on work of its own.
func (c *conn) answer(id jsonrpc.ID, result any, handlerErr error) func() {
	if !c.claimResponse(id) {
		// A cancellation answered this request while the handler was still
		// working. Its answer is late and is dropped: one request, one response.
		return nil
	}
	return func() { c.writeResponse(id, result, handlerErr) }
}

// writeResponse writes a response for a request already claimed.
//
// The context is the connection's rather than the request's, deliberately: the
// request's has just been cancelled in the case that matters most, and a cancelled
// turn still owes the peer an answer.
func (c *conn) writeResponse(id jsonrpc.ID, result any, handlerErr error) {
	if result == nil && handlerErr == nil {
		// A request handler that returns nothing is a bug in the dispatch table,
		// not something to send an empty result for: every request in the schema
		// has a response type, however few properties it has.
		handlerErr = newError(ErrorCodeInternalError, "no result and no error")
	}
	response, err := jsonrpc2.NewResponse(id, result, c.wireError(handlerErr))
	if err != nil {
		// The result could not be encoded, which the peer cannot be told in the
		// same breath as being told the result.
		c.logger.Error("acp: encoding a result failed", slog.Any("error", err))
		response, err = jsonrpc2.NewResponse(id, nil,
			c.wireError(newError(ErrorCodeInternalError, "the result could not be encoded")))
		if err != nil {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.base), cancelRequestTimeout)
	defer cancel()
	if err := c.transport.Write(ctx, response); err != nil {
		// Through the same terminal path as any other failed write. A connection
		// whose output side has failed can otherwise go on accepting requests it
		// can never answer, and nothing would ever record why.
		c.logger.Error("acp: writing a response failed", slog.Any("error", err))
		_ = c.writeFailure(err)
	}
}

// wireError decides what a peer is told about a failure.
//
// A handler that returns an *Error has chosen the code, the message and the data
// deliberately, and all three are sent. A handler that returns anything else gets
// a stable internal error, and the detail is logged locally: handler errors
// routinely carry paths, hostnames and internal identifiers, and the peer is not
// entitled to them.
func (c *conn) wireError(err error) error {
	if err == nil {
		return nil
	}
	var chosen *Error
	if errors.As(err, &chosen) {
		wire := &jsonrpc2.WireError{Code: int64(chosen.Code), Message: chosen.Message}
		switch data, present := chosen.Data.Get(); {
		case present:
			wire.Data = data
		case chosen.Data.IsNull():
			// Absent and null are different things to say, and Opt exists to keep
			// them different. A relay that dropped the null would send the peer a
			// different error from the one it was given.
			wire.Data = json.RawMessage("null")
		}
		return wire
	}
	c.logger.Error("acp: a handler failed", slog.Any("error", err))
	return &jsonrpc2.WireError{
		Code:    int64(ErrorCodeInternalError),
		Message: ErrorCodeInternalError.String(),
	}
}

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
		failure := &Error{Code: errorCodeOf(wire.Code), Message: wire.Message}
		switch {
		case len(wire.Data) == 0:
			// Absent, which is the zero value and needs saying no other way.
		case string(wire.Data) == "null":
			// The peer said null, which is a value it chose. Wrapping it as a
			// present raw null would report data the peer did not send.
			failure.Data = OptNull[json.RawMessage]()
		default:
			failure.Data = OptValue(wire.Data)
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

// errorCodeOf narrows a JSON-RPC code to the schema's int32.
//
// The schema types every arm of the code union as an int32, so a wider value is a
// peer that is not following it. Truncating would report a code the peer did not
// send, which is worse than saying the failure was internal to it: an unknown
// in-range code is valid and survives, and an out-of-range one is not a code.
func errorCodeOf(code int64) ErrorCode {
	if code < math.MinInt32 || code > math.MaxInt32 {
		return ErrorCodeInternalError
	}
	return ErrorCode(code)
}

// writeFailure translates a transport write failure.
//
// A failed write ends the logical connection, which is the Connection contract's
// last clause and the reason this does more than return the error.
func (c *conn) writeFailure(err error) error {
	if errors.Is(err, ErrConnectionClosed) {
		return c.terminalError()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The caller's own context, not the connection's health.
		return err
	}
	c.endReading(err)
	return err
}

// close ends the connection, and is idempotent. It does not wait: the messages
// already accepted are still delivered, and wait is what observes that finishing.
//
// It reports the connection's terminal error, which is the same value wait
// reports. One policy rather than two: releasing what the connection owns is its
// last obligation, and a transport that could not be closed — a subprocess that
// is still running — is a failure of that obligation and not a detail for a log.
func (c *conn) close() error {
	c.endReading(nil)
	<-c.readEnded // terminal is written before this is closed
	return c.terminal
}

// endReading records the terminal condition exactly once and stops the read side.
//
// It does not end the connection: orderedLoop does that, once it has delivered
// everything the read side already accepted.
func (c *conn) endReading(cause error) {
	c.closeOnce.Do(func() {
		// A clean end of stream is not a failure. Neither is a read that was
		// unblocked by this side closing the transport.
		switch {
		case errors.Is(cause, io.EOF):
			cause = nil
		case errors.Is(cause, context.Canceled) && c.base.Err() != nil:
			cause = nil
		case errors.Is(cause, ErrConnectionClosed):
			cause = nil
		}
		// The transport first, so that a peer blocked writing to us is released;
		// then the base context, which stops every handler still running. The
		// messages already queued are drained under a context of their own.
		if err := c.transport.Close(); err != nil {
			c.logger.Warn("acp: closing the transport failed", slog.Any("error", err))
			if cause == nil {
				// Nothing else went wrong, so this is what went wrong. A command
				// transport reports here that it could not reap the agent it
				// started, and a connection that answered "closed cleanly" to that
				// would be reporting a process that is still running as gone.
				cause = err
			}
		}
		c.terminal = cause

		c.cancelBase()
		close(c.readEnded)
	})
}

// ended reports whether the connection is over, which is the read side having
// finished rather than the delivery queue having drained.
//
// The registry asks this to decide what to still list as open, and a connection
// that will accept nothing further is not open, however much is still being
// handed to the callers that were already waiting.
func (c *conn) ended() bool {
	select {
	case <-c.readEnded:
		return true
	default:
		return false
	}
}

// wait blocks until the connection has ended and everything it owns has stopped,
// then reports the terminal error.
//
// Every caller gets the same value every time. A terminal condition that reported
// differently depending on who asked first would be unusable for deciding whether
// to reconnect.
func (c *conn) wait() error {
	<-c.done
	c.work.Wait()
	return c.terminal
}

// terminalError is what an operation on an ended connection returns.
func (c *conn) terminalError() error {
	<-c.done
	if c.terminal != nil {
		return c.terminal
	}
	return ErrConnectionClosed
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
