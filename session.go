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
var ErrPromptInProgress = errors.New("acp: prompt already in progress for this session")

// A ClientSession is one conversation, as the client sees it.
//
// It binds the session identifier and nothing else. Session identifiers appear
// across many protocol operations, so binding one prevents mismatched identifiers
// without expanding the handle's lifetime or responsibility.
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

func (s *ClientSession) ID() SessionID { return s.id }

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
	if params != nil {
		if err := conn.Peer().permitsPromptContent(params.Prompt); err != nil {
			return nil, err
		}
	}
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
//
// No capability gates it: an agent offers modes by returning them from
// [ClientConn.NewSession], so an agent that offered none has nothing to set and
// answers method-not-found.
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

// SetConfigOption sets one of the session configuration options the agent
// offered. Which options exist, and which values they take, come from the agent:
// session/new returns them and every answer here returns the full set again.
func (s *ClientSession) SetConfigOption(
	ctx context.Context,
	params *SetConfigOptionParams,
) (*SetSessionConfigOptionResponse, error) {
	if params == nil {
		return nil, paramsRequired("SetConfigOption", "ConfigID and Value")
	}
	if err := s.conn.Peer().permitsConfigOptionValue(params.Value); err != nil {
		return nil, err
	}
	request := &SetSessionConfigOptionRequest{
		SessionID: s.id,
		ConfigID:  params.ConfigID,
		Value:     params.Value,
		Meta:      params.Meta,
	}
	response := new(SetSessionConfigOptionResponse)
	if err := s.conn.call(ctx, methodSessionSetConfigOption, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// Close ends the session and frees what the agent holds for it.
//
// The agent must cancel any work still running for the session before it frees
// anything, so an outstanding [ClientSession.Prompt] answers with the cancelled
// stop reason rather than being abandoned. After success, the connection removes
// this handle from its identity cache. Callers should discard it; a later
// operation that reopens the same identifier receives a new handle.
//
// Gated on agentCapabilities.sessionCapabilities.close.
func (s *ClientSession) Close(ctx context.Context, params *CloseParams) (*CloseSessionResponse, error) {
	if err := s.conn.Peer().permits(methodSessionClose); err != nil {
		return nil, err
	}
	request := &CloseSessionRequest{SessionID: s.id}
	if params != nil {
		request.Meta = params.Meta
	}
	response := new(CloseSessionResponse)
	if err := s.conn.call(ctx, methodSessionClose, request, response); err != nil {
		return nil, err
	}
	s.conn.sessions.forget(s.id)
	return response, nil
}

// NewSession opens a conversation.
//
// It returns the response as well as the handle because the response carries more
// than an identifier — modes, configuration options and _meta — and a handle alone
// would put those out of reach.
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
	if err := c.checkSessionSetup(params.Cwd, params.McpServers, params.AdditionalDirectories); err != nil {
		return nil, nil, err
	}
	response := new(NewSessionResponse)
	if err := c.call(ctx, methodSessionNew, params, response); err != nil {
		return nil, nil, err
	}
	return c.session(response.SessionID), response, nil
}

// LoadSession reopens a conversation the agent already has, replaying its history
// as [SessionNotification] updates before it answers.
//
// Loading an identifier this connection has already seen returns the handle it
// already has. Loading it through a different connection returns a different one:
// handles never silently move between transports.
//
// Gated on agentCapabilities.loadSession.
func (c *ClientConn) LoadSession(
	ctx context.Context,
	params *LoadSessionRequest,
) (*ClientSession, *LoadSessionResponse, error) {
	if params == nil {
		return nil, nil, paramsRequired("LoadSession", "SessionID")
	}
	// The outbound half of the gate. A call the peer never advertised is refused
	// here rather than sent and refused there: the answer would be the same, and
	// asking wastes a round trip while making a developer read a wire trace to
	// find out what they forgot.
	if err := c.Peer().permits(methodSessionLoad); err != nil {
		return nil, nil, err
	}
	if err := c.checkSessionSetup(params.Cwd, params.McpServers, params.AdditionalDirectories); err != nil {
		return nil, nil, err
	}
	response := new(LoadSessionResponse)
	if err := c.call(ctx, methodSessionLoad, params, response); err != nil {
		return nil, nil, err
	}
	return c.session(params.SessionID), response, nil
}

// ResumeSession reopens a session the agent still has, without replaying it.
//
// That is what the schema says separates it from [ClientConn.LoadSession]: resume
// is for an agent that can continue a conversation but does not implement handing
// its history back.
//
// Gated on agentCapabilities.sessionCapabilities.resume.
func (c *ClientConn) ResumeSession(
	ctx context.Context,
	params *ResumeSessionRequest,
) (*ClientSession, *ResumeSessionResponse, error) {
	if params == nil {
		return nil, nil, paramsRequired("ResumeSession", "SessionID")
	}
	if err := c.Peer().permits(methodSessionResume); err != nil {
		return nil, nil, err
	}
	if err := c.checkSessionSetup(params.Cwd, params.McpServers, params.AdditionalDirectories); err != nil {
		return nil, nil, err
	}
	response := new(ResumeSessionResponse)
	if err := c.call(ctx, methodSessionResume, params, response); err != nil {
		return nil, nil, err
	}
	return c.session(params.SessionID), response, nil
}

// ListSessions reports the sessions the agent has stored.
//
// It is paginated: a response carrying NextCursor has more behind it, and passing
// that cursor back asks for the next page. The pages are the agent's own — this
// does not gather them, because a caller that wants one page should not pay for
// all of them.
//
// Gated on agentCapabilities.sessionCapabilities.list.
func (c *ClientConn) ListSessions(
	ctx context.Context,
	params *ListSessionsRequest,
) (*ListSessionsResponse, error) {
	if params == nil {
		params = &ListSessionsRequest{}
	}
	if err := c.Peer().permits(methodSessionList); err != nil {
		return nil, err
	}
	response := new(ListSessionsResponse)
	if err := c.call(ctx, methodSessionList, params, response); err != nil {
		return nil, err
	}
	return response, nil
}

// DeleteSession removes a stored session, which the schema describes as removing
// it from [ClientConn.ListSessions].
//
// It takes an identifier rather than a handle because the session it names is one
// this connection may never have opened. If it did, the handle is forgotten: it
// would otherwise name a session the agent no longer has.
//
// Gated on agentCapabilities.sessionCapabilities.delete.
func (c *ClientConn) DeleteSession(
	ctx context.Context,
	params *DeleteSessionRequest,
) (*DeleteSessionResponse, error) {
	if params == nil {
		return nil, paramsRequired("DeleteSession", "SessionID")
	}
	if err := c.Peer().permits(methodSessionDelete); err != nil {
		return nil, err
	}
	response := new(DeleteSessionResponse)
	if err := c.call(ctx, methodSessionDelete, params, response); err != nil {
		return nil, err
	}
	c.sessions.forget(params.SessionID)
	return response, nil
}

// Authenticate performs the authentication an agent asked for, which is how a
// client answers [ErrAuthRequired].
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

// The three requests that open or reopen a session carry the same workspace
// parameters, and the specification constrains all of them: an http or sse MCP
// server and additionalDirectories are gated on what the agent advertised, and
// every path must be absolute.
func (c *ClientConn) checkSessionSetup(cwd string, servers []McpServer, directories []string) error {
	if err := c.Peer().permitsSessionSetup(servers, directories); err != nil {
		return err
	}
	if err := absolutePath("cwd", cwd); err != nil {
		return err
	}
	return absoluteDirectories(directories)
}

func (c *ClientConn) session(id SessionID) *ClientSession {
	handle, within := c.sessions.lookup(id, c.limits.SessionHandles, func(id SessionID) *ClientSession {
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

func (s *AgentSession) ID() SessionID { return s.id }

func (s *AgentSession) Conn() *AgentConn { return s.conn }

// Update sends the client one piece of a turn's output.
//
// A client keeps accepting these after it has sent a cancellation, because an
// agent may still have final tool-call updates to report before it answers the
// prompt.
func (s *AgentSession) Update(ctx context.Context, params *SessionUpdateParams) error {
	if params == nil {
		return paramsRequired("Update", "Update")
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
// A client that cancels the turn while this is outstanding answers it with the
// cancelled outcome rather than dropping it, so this returns an outcome either
// way and never hangs on a turn that has ended.
func (s *AgentSession) RequestPermission(
	ctx context.Context,
	params *RequestPermissionParams,
) (*RequestPermissionResponse, error) {
	if params == nil {
		return nil, paramsRequired("RequestPermission", "ToolCall and Options")
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
	handle, within := c.sessions.lookup(id, c.limits.SessionHandles, func(id SessionID) *AgentSession {
		return &AgentSession{id: id, conn: c}
	})
	if !within {
		c.tooManySessions()
	}
	return handle
}

// The handler receives no handle because this is the call that creates one.
func (c *AgentConn) newSession(ctx context.Context, request *jsonrpc.Request) (any, error) {
	result, err := dispatchConnCall(ctx, c, request, c.agent.config.NewSession)
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

// closeSession serves session/close, whose obligation is the schema's rather than
// the application's: "the agent **must** cancel any ongoing work related to the
// session (treat it as if `session/cancel` was called) and then free up any
// resources associated with the session".
//
// The cancellation is the connection's because the turn is: an application that
// had to remember to cancel its own turn before freeing it would forget, and the
// prompt still owes the client the cancelled stop reason. The application's own
// Cancel handler is deliberately not invoked — it is about to be told something
// strictly more specific, and one event should not arrive twice.
//
// The handle is forgotten only after the handler returns, because the handler is
// given it, and only when the handler succeeded, because a close that failed
// closed nothing.
func (c *AgentConn) closeSession(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[CloseSessionRequest](request)
	if err != nil {
		return nil, err
	}
	if c.agent.config.CloseSession == nil {
		return nil, methodNotImplemented(request.Method)
	}
	c.cancelTurn(params.SessionID)

	response, err := c.agent.config.CloseSession(ctx, c.session(params.SessionID), params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	c.sessions.forget(params.SessionID)
	return response, nil
}

// deleteSession binds the session named by the request before handing it to the
// application. The session need not have been cached already: its identifier is
// nevertheless the protocol scope, and the successful deletion is what releases
// the handle again.
func (c *AgentConn) deleteSession(ctx context.Context, request *jsonrpc.Request) (any, error) {
	handle := c.agent.config.DeleteSession
	if handle == nil {
		return nil, methodNotImplemented(request.Method)
	}
	params, err := decodeParams[DeleteSessionRequest](request)
	if err != nil {
		return nil, err
	}
	response, err := handle(ctx, c.session(params.SessionID), params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	c.sessions.forget(params.SessionID)
	return response, nil
}

// A sessionRequest is a request that names the session it belongs to, which is
// what lets one dispatch path hand a handle to every handler that needs one.
type sessionRequest interface {
	sessionID() SessionID
}

func (x *LoadSessionRequest) sessionID() SessionID            { return x.SessionID }
func (x *SetSessionModeRequest) sessionID() SessionID         { return x.SessionID }
func (x *SetSessionConfigOptionRequest) sessionID() SessionID { return x.SessionID }
func (x *ResumeSessionRequest) sessionID() SessionID          { return x.SessionID }

func dispatchSessionCall[Request sessionRequest, Response any](
	ctx context.Context,
	c *AgentConn,
	request *jsonrpc.Request,
	handle func(context.Context, *AgentSession, Request) (*Response, error),
) (any, error) {
	if handle == nil {
		return nil, methodNotImplemented(request.Method)
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
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}
