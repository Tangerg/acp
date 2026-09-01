package acp

import (
	"context"
	"errors"

	"github.com/Tangerg/acp/jsonrpc"
)

// ErrPromptInProgress refuses a second concurrent prompt on one session.
//
// The protocol has no way to say which turn a session/cancel means: the
// notification carries only a session identifier — no turn and no request
// identifier. That is only coherent if a session has at most one active turn, so
// a second overlapping prompt is refused locally rather than left to scheduling,
// and pending permission requests can then be indexed by session without
// ambiguity.
//
// A session is safe for concurrent use; that is a separate question from whether
// two prompts may overlap, and the answer to the first is yes while the answer to
// the second is no.
var ErrPromptInProgress = errors.New("acp: this session already has a prompt in flight")

// A ClientSession is one conversation, as the client sees it.
//
// It binds the session identifier and nothing else. Thirty-two definitions in the
// schema carry a sessionId property, and threading that string by hand across the
// operations that need it is the same mistake as threading a terminal identifier
// through five functions.
//
// Handles are connection-bound. A [SessionID] outlives the connection it came
// from, but a handle does not: LoadSession on a new connection returns a new
// handle. A handle that silently re-pointed at another transport would be a
// lifetime nobody can reason about, so callers keep the identifier and ask for a
// fresh handle.
type ClientSession struct {
	id   SessionID
	conn *ClientConn
}

// ID is the identifier the agent gave this session.
func (s *ClientSession) ID() SessionID { return s.id }

// Conn is the connection this session belongs to.
func (s *ClientSession) Conn() *ClientConn { return s.conn }

// Prompt runs one turn and blocks until it ends.
//
// While it is outstanding the agent streams session/update notifications and may
// call back into this client — for permission before a sensitive tool call, and
// for the filesystem and terminal operations that do the work. That is why both
// peers serve requests: an agent answering a prompt is simultaneously a caller.
//
// Cancelling ctx cancels the JSON-RPC request and returns ctx.Err(). It does not
// end the turn: [ClientSession.Cancel] does that, and the two are different
// operations for a reason the protocol insists on. See [ClientSession.Cancel].
//
// The turn outlives this call when that happens. The agent still owes an answer,
// and the session is not free for the next prompt until the connection accepts
// that answer. A caller who wants the session back promptly should call Cancel,
// which is the operation that ends a turn.
func (s *ClientSession) Prompt(ctx context.Context, params *PromptParams) (*PromptResponse, error) {
	conn := s.conn
	generation, claimed := conn.turns.begin(s.id)
	if !claimed {
		return nil, ErrPromptInProgress
	}

	request := &PromptRequest{SessionID: s.id}
	if params != nil {
		request.Prompt = params.Prompt
		request.Meta = params.Meta
	}

	result := new(PromptResponse)
	finish := func() { conn.turns.complete(s.id, generation) }
	call, err := conn.send(ctx, methodSessionPrompt, request, func(response *jsonrpc.Response) error {
		defer finish()
		return decodeResponse(response, result)
	}, finish)
	if err != nil {
		finish()
		return nil, err
	}

	select {
	case err := <-call.completed:
		if err != nil {
			return nil, err
		}
		return result, nil

	case <-conn.over():
		// An answer already in hand is an answer; see link.await.
		select {
		case err := <-call.completed:
			if err != nil {
				return nil, err
			}
			return result, nil
		default:
		}
		return nil, conn.failure()

	case <-ctx.Done():
		select {
		case err := <-call.completed:
			if err != nil {
				return nil, err
			}
			return result, nil
		default:
		}
		// This caller has stopped waiting. The turn has not stopped running: the
		// agent owes an answer and the protocol says what it is. Tell the agent,
		// on a budget of its own, and leave a waiter behind — the session is free
		// for the next prompt when that answer arrives and not before.
		//nolint:contextcheck // deliberate; the notification has a budget of its own.
		conn.cancelRemotely(call.id)
		return nil, ctx.Err()
	}
}

// Cancel ends the current turn.
//
// It is a notification, and sending it does not end the turn by itself: the
// protocol requires the agent to answer the outstanding session/prompt with the
// cancelled stop reason, and requires this client to keep accepting session/update
// until it does — the agent may still have final tool-call updates to report.
//
// This is not the same as cancelling a Prompt's context. That cancels one
// JSON-RPC request and stops this side waiting; this cancels a turn and leaves the
// request outstanding, on purpose, so that the agent can answer it.
func (s *ClientSession) Cancel(ctx context.Context, params *CancelParams) error {
	// The pending permission requests are claimed and answered first — before the
	// notification goes out and before this returns. See cancel.go.
	cancellation := s.conn.turns.beginCancellation(s.id)
	for _, id := range cancellation.pending {
		settled := s.conn.requests.settlement(id)
		//nolint:contextcheck // the answer is the connection's obligation, not this caller's.
		if write := s.conn.answerCancelled(id); write != nil {
			write()
			continue
		}
		// The permission handler may have won the answer claim immediately before
		// cancellation. Its response still has to reach the wire before
		// session/cancel; a claimed answer is not yet an observed one.
		if settled != nil {
			<-settled
		}
	}
	// And the turn is held until the notification is on the wire. It names only
	// the session, so a prompt that started before it went out would be the turn
	// the agent applies it to.
	defer s.conn.turns.finishCancellation(s.id, cancellation.generation)

	notification := &CancelNotification{SessionID: s.id}
	if params != nil {
		notification.Meta = params.Meta
	}
	return s.conn.notify(ctx, methodSessionCancel, notification)
}

// SetMode switches the agent's mode for this session.
func (s *ClientSession) SetMode(ctx context.Context, params *SetModeParams) (*SetSessionModeResponse, error) {
	request := &SetSessionModeRequest{SessionID: s.id}
	if params != nil {
		request.ModeID = params.ModeID
		request.Meta = params.Meta
	}
	response := new(SetSessionModeResponse)
	if err := s.conn.call(ctx, methodSessionSetMode, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// NewSession creates a conversation.
//
// It returns three things because the response carries more than an identifier —
// modes, config options and _meta — and returning only a handle would make those
// unreachable. Three results is mildly ugly and strictly lossless.
//
// An agent that requires authentication answers this with -32000, which is control
// flow rather than failure:
//
//	session, result, err := conn.NewSession(ctx, params)
//	if errors.Is(err, acp.ErrAuthRequired) {
//		// Expected. Authenticate, then retry.
//	}
func (c *ClientConn) NewSession(
	ctx context.Context,
	params *NewSessionRequest,
) (*ClientSession, *NewSessionResponse, error) {
	if params == nil {
		params = &NewSessionRequest{}
	}
	response := new(NewSessionResponse)
	if err := c.call(ctx, methodSessionNew, params, response); err != nil {
		return nil, nil, err
	}
	return c.session(response.SessionID), response, nil
}

// LoadSession reopens a conversation the agent already has, replaying its history
// as session/update notifications.
//
// A connection keeps one canonical handle per session identifier, so loading an
// identifier already seen on this connection returns that handle and all callers
// share the same turn invariant. Loading it through a new connection returns a
// new handle, because handles never silently move between transports.
func (c *ClientConn) LoadSession(
	ctx context.Context,
	params *LoadSessionRequest,
) (*ClientSession, *LoadSessionResponse, error) {
	if params == nil {
		return nil, nil, errors.New("acp: LoadSession needs params: the session to load is in them")
	}
	// The outbound half of the gate. A call the peer never advertised is refused
	// here rather than sent and refused there: the answer would be the same, and
	// asking wastes a round trip while making a developer read a wire trace to
	// find out what they forgot.
	if err := c.Peer().permits(methodSessionLoad); err != nil {
		return nil, nil, err
	}
	response := new(LoadSessionResponse)
	if err := c.call(ctx, methodSessionLoad, params, response); err != nil {
		return nil, nil, err
	}
	return c.session(params.SessionID), response, nil
}

// Authenticate performs the authentication an agent asked for.
//
// The method identifier is one of those the agent advertised in its initialize
// response, which [ClientConn.Peer] carries. A terminal method is not one of
// them: the schema says the client "MUST NOT pass this method to authenticate",
// because it is performed by running the agent again in an interactive terminal
// rather than by calling it.
func (c *ClientConn) Authenticate(
	ctx context.Context,
	params *AuthenticateRequest,
) (*AuthenticateResponse, error) {
	if params == nil {
		params = &AuthenticateRequest{}
	}
	if err := c.Peer().authenticates(params.MethodID); err != nil {
		return nil, err
	}
	response := new(AuthenticateResponse)
	if err := c.call(ctx, methodAuthenticate, params, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *ClientConn) session(id SessionID) *ClientSession {
	handle, within := c.sessions.lookup(id, func(id SessionID) *ClientSession {
		return &ClientSession{id: id, conn: c}
	})
	if !within {
		c.tooManySessions()
	}
	return handle
}

// An AgentSession is the same conversation as the agent sees it.
//
// A different set of operations, so a different type. A client never calls
// RequestPermission and an agent never calls Prompt; one type carrying both method
// sets would make those calls compile and fail at runtime, which is a worse place
// to find out.
//
// An agent never constructs one: the connection hands it to the handlers whose
// requests carry a session identifier. The handle is valid for as long as the
// session is, not merely for the handler call — an agent that spawns work for a
// turn keeps the handle to send session/update from it, and that is the ordinary
// case rather than an escape. What it must not outlive is the connection: calling
// through it after Close returns [ErrConnectionClosed].
type AgentSession struct {
	id   SessionID
	conn *AgentConn
}

// ID is the identifier of the session this handle names.
func (s *AgentSession) ID() SessionID { return s.id }

// Conn is the connection this session belongs to.
func (s *AgentSession) Conn() *AgentConn { return s.conn }

// Update sends the client one piece of a turn's output.
//
// It is a notification, so there is no response to return. Clients keep accepting
// these after sending session/cancel, because an agent may still have final
// tool-call updates to report.
func (s *AgentSession) Update(ctx context.Context, params *SessionUpdateParams) error {
	if params == nil {
		return errors.New("acp: Update needs params: the update is in them")
	}
	if err := s.conn.awaitHandshake(ctx, methodSessionUpdate); err != nil {
		return err
	}
	notification := &SessionNotification{
		SessionID: s.id,
		Update:    params.Update,
		Meta:      params.Meta,
	}
	return s.conn.notify(ctx, methodSessionUpdate, notification)
}

// RequestPermission asks the user to approve a tool call.
//
// If the client cancels the turn while this is outstanding, it must answer with
// the cancelled outcome rather than dropping the request. That obligation is the
// client's, and this side sees it as an ordinary response.
func (s *AgentSession) RequestPermission(
	ctx context.Context,
	params *RequestPermissionParams,
) (*RequestPermissionResponse, error) {
	if params == nil {
		return nil, errors.New("acp: RequestPermission needs params: the tool call and the options are in them")
	}
	if err := s.conn.awaitHandshake(ctx, methodSessionRequestPermission); err != nil {
		return nil, err
	}
	request := &RequestPermissionRequest{
		SessionID: s.id,
		ToolCall:  params.ToolCall,
		Options:   params.Options,
		Meta:      params.Meta,
	}
	response := new(RequestPermissionResponse)
	if err := s.conn.call(ctx, methodSessionRequestPermission, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *AgentConn) session(id SessionID) *AgentSession {
	handle, within := c.sessions.lookup(id, func(id SessionID) *AgentSession {
		return &AgentSession{id: id, conn: c}
	})
	if !within {
		c.tooManySessions()
	}
	return handle
}

// newSession serves session/new and builds the handle from the identifier the
// handler returned.
//
// The handler receives no handle because this is the call that creates one.
func (c *AgentConn) newSession(ctx context.Context, request *jsonrpc.Request) (any, error) {
	result, err := dispatchCall(ctx, request, c.agent.config.NewSession)
	if err != nil {
		return nil, err
	}
	response, ok := result.(*NewSessionResponse)
	if !ok {
		return nil, newError(ErrorCodeInternalError, "session/new returned %T", result)
	}
	c.session(response.SessionID)
	return response, nil
}

// A sessionRequest is a request that names the session it belongs to, which is
// what lets one dispatch path hand a handle to every handler that needs one.
type sessionRequest interface {
	sessionID() SessionID
}

func (x *LoadSessionRequest) sessionID() SessionID    { return x.SessionID }
func (x *SetSessionModeRequest) sessionID() SessionID { return x.SessionID }

// dispatchSessionCall serves a request whose handler takes a session handle.
func dispatchSessionCall[Request sessionRequest, Response any](
	ctx context.Context,
	c *AgentConn,
	request *jsonrpc.Request,
	handle func(context.Context, *AgentSession, Request) (*Response, error),
) (any, error) {
	if handle == nil {
		return nil, newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
	}
	params, err := decodeParams[Request](request)
	if err != nil {
		return nil, err
	}
	response, err := handle(ctx, c.session((*params).sessionID()), *params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, newError(ErrorCodeInternalError, "the handler for %s returned nothing", request.Method)
	}
	return response, nil
}
