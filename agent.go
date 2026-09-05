package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"

	"github.com/Tangerg/acp/jsonrpc"
)

// AgentConfig defines an agent's identity, handlers, and capability
// advertisement. NewSession, Prompt, and Cancel are required. Capability-gated
// optional handlers derive their advertisement unless Capabilities is set.
type AgentConfig struct {
	// Info identifies this agent during initialize.
	Info *Implementation
	// Meta is attached to this agent's initialize response.
	Meta Meta
	// Logger is where a handler's failure detail goes, since the peer is not
	// entitled to it. nil means discard; see [ClientConfig.Logger].
	Logger *slog.Logger

	// AuthMethods is what an agent offers a client that must authenticate. An
	// empty list says none is needed.
	AuthMethods []AuthMethod

	// Baseline. NewAgent fails if any is nil, because an agent that cannot answer
	// these cannot complete a turn.
	//
	// A handler is given the handle its method is scoped to, so that serving one
	// does not mean re-deriving where it belongs. A session-scoped method gets an
	// [AgentSession]; a connection-scoped one gets the [AgentConn] the request
	// arrived on, which is the only way a handler with no session can call the
	// client back at all — an elicitation during authentication is the
	// specification's own example.
	NewSession func(ctx context.Context, conn *AgentConn, request *NewSessionRequest) (*NewSessionResponse, error)
	Prompt     func(ctx context.Context, session *AgentSession, request *PromptRequest) (*PromptResponse, error)
	Cancel     func(ctx context.Context, session *AgentSession, notification *CancelNotification)

	// Authenticate answers an agent-managed method listed in AuthMethods. It is
	// required when any listed method is agent-managed, and nil means this agent
	// requires no authentication.
	Authenticate func(ctx context.Context, conn *AgentConn, request *AuthenticateRequest) (*AuthenticateResponse, error)

	// LoadSession is gated on the loadSession capability. Setting it advertises
	// that capability.
	LoadSession func(ctx context.Context, session *AgentSession, request *LoadSessionRequest) (*LoadSessionResponse, error)

	// SetMode switches a session's mode. It is gated by data rather than by a
	// capability: an agent offers modes by returning them from session/new, and a
	// nil handler here means it offers none.
	SetMode func(ctx context.Context, session *AgentSession, request *SetSessionModeRequest) (*SetSessionModeResponse, error)

	// SetConfigOption sets one of the session configuration options this agent
	// offered. Gated by data exactly as SetMode is.
	SetConfigOption func(
		ctx context.Context,
		session *AgentSession,
		request *SetSessionConfigOptionRequest,
	) (*SetSessionConfigOptionResponse, error)

	// Logout ends the authenticated session. Setting it advertises
	// agentCapabilities.auth.logout.
	Logout func(ctx context.Context, conn *AgentConn, request *LogoutRequest) (*LogoutResponse, error)

	// The session lifecycle, each gated on its own property of
	// agentCapabilities.sessionCapabilities and each advertised by being set.
	//
	// ListSessions has no session and therefore takes the connection. DeleteSession
	// names the session it removes and takes that session, including when this
	// connection had not opened it before: scope comes from the wire request, not
	// from whether a handle happened to be cached already.
	ListSessions  func(ctx context.Context, conn *AgentConn, request *ListSessionsRequest) (*ListSessionsResponse, error)
	DeleteSession func(
		ctx context.Context,
		session *AgentSession,
		request *DeleteSessionRequest,
	) (*DeleteSessionResponse, error)

	// ResumeSession reopens a session without replaying it, which is what the
	// schema says distinguishes it from session/load.
	ResumeSession func(
		ctx context.Context,
		session *AgentSession,
		request *ResumeSessionRequest,
	) (*ResumeSessionResponse, error)

	// CloseSession frees what a session holds. The connection cancels the session's
	// turn before this handler runs.
	CloseSession func(
		ctx context.Context,
		session *AgentSession,
		request *CloseSessionRequest,
	) (*CloseSessionResponse, error)

	// CallFallback and NotifyFallback receive extension methods, exactly as on the
	// client side: the extension contract is symmetric because both directions can
	// send extension messages and both can receive them.
	CallFallback   func(ctx context.Context, request *ExtRequest) (json.RawMessage, error)
	NotifyFallback func(ctx context.Context, notification *ExtNotification)

	// Capabilities is the complete desired advertisement, never a patch, and never
	// inferred from the client callbacks an agent happens to be able to make: what
	// an agent advertises is what it implements. See [ClientConfig.Capabilities].
	Capabilities *AgentCapabilities

	// Limits bounds the protocol state each of this agent's connections will hold.
	// The zero value takes every default; see [Limits].
	Limits Limits
}

// An Agent owns the model-facing handlers and capabilities shared by its
// connections. One agent may serve many clients.
type Agent struct {
	config       AgentConfig
	capabilities AgentCapabilities
	auth         authenticationMethods
	conns        registry[*AgentConn]
}

// NewAgent builds an agent, or reports why the configuration cannot be served.
func NewAgent(config *AgentConfig) (*Agent, error) {
	if config == nil {
		config = &AgentConfig{}
	}
	var missing []string
	if config.NewSession == nil {
		missing = append(missing, "NewSession")
	}
	if config.Prompt == nil {
		missing = append(missing, "Prompt")
	}
	if config.Cancel == nil {
		missing = append(missing, "Cancel")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("acp: AgentConfig is missing required handlers: %v", missing)
	}
	if err := config.Limits.check(); err != nil {
		return nil, err
	}
	capabilities, err := config.resolveCapabilities()
	if err != nil {
		return nil, err
	}
	auth, err := newAuthenticationMethods(config.AuthMethods, config.Authenticate != nil)
	if err != nil {
		return nil, err
	}
	cloned := config.clone()
	cloned.AuthMethods = nil
	return &Agent{config: cloned, capabilities: capabilities, auth: auth}, nil
}

func (config *AgentConfig) resolveCapabilities() (AgentCapabilities, error) {
	derived := AgentCapabilities{LoadSession: config.LoadSession != nil}
	if config.Logout != nil {
		derived.Auth.Logout = OptValue(LogoutCapabilities{})
	}
	if config.ListSessions != nil {
		derived.SessionCapabilities.List = OptValue(SessionListCapabilities{})
	}
	if config.DeleteSession != nil {
		derived.SessionCapabilities.Delete = OptValue(SessionDeleteCapabilities{})
	}
	if config.ResumeSession != nil {
		derived.SessionCapabilities.Resume = OptValue(SessionResumeCapabilities{})
	}
	if config.CloseSession != nil {
		derived.SessionCapabilities.Close = OptValue(SessionCloseCapabilities{})
	}
	if config.Capabilities == nil {
		return derived, nil
	}

	stated := deepCopy(*config.Capabilities)
	exceeded := gates.exceeded(PeerInfo{AgentCapabilities: stated}, sideAgent, config.implements)
	if len(exceeded) > 0 {
		return AgentCapabilities{}, fmt.Errorf(
			"acp: AgentConfig.Capabilities advertises unsupported methods: %v", exceeded)
	}
	return stated, nil
}

// See [ClientConfig.implements].
func (config *AgentConfig) implements(method string) bool {
	switch method {
	case methodInitialize, methodSessionNew, methodSessionPrompt, methodSessionCancel:
		return true // baseline, and NewAgent has already refused a nil handler
	case methodAuthenticate:
		return config.Authenticate != nil
	case methodSessionLoad:
		return config.LoadSession != nil
	case methodSessionSetMode:
		return config.SetMode != nil
	case methodSessionSetConfigOption:
		return config.SetConfigOption != nil
	case methodLogout:
		return config.Logout != nil
	case methodSessionList:
		return config.ListSessions != nil
	case methodSessionDelete:
		return config.DeleteSession != nil
	case methodSessionResume:
		return config.ResumeSession != nil
	case methodSessionClose:
		return config.CloseSession != nil
	default:
		return false
	}
}

func (config *AgentConfig) clone() AgentConfig {
	copied := *config
	copied.Info = deepCopy(config.Info)
	copied.Meta = deepCopy(config.Meta)
	copied.Capabilities = deepCopy(config.Capabilities)
	return copied
}

// Connect accepts a connection and returns once the read loop is running, before
// any client has sent initialize.
//
// It cannot wait for a handshake it does not control — this side answers one, it
// does not perform one — which is the asymmetry with [Client.Connect]. Until
// initialize arrives the connection answers every other method with -32600. An
// initialize accepted on the connection makes every later attempt -32600. If an
// attempt is rejected before acceptance, queued or later corrected attempts are
// evaluated in wire order without requiring a reconnect.
//
// The context scopes setup only. Lifetime is owned by [AgentConn.Close] and
// observed by [AgentConn.Wait]; [Agent.Run] is the exception and says so.
func (a *Agent) Connect(ctx context.Context, transport Transport) (*AgentConn, error) {
	stream, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	conn := &AgentConn{connection: newConnection(), agent: a}
	conn.link = newLink(stream, conn, a.config.Logger, a.config.Limits)
	conn.run()

	a.conns.add(conn)
	return conn, nil
}

// Run serves one connection until its context is cancelled or the connection
// ends, then closes it.
//
// This is the one place a context owns a connection's lifetime, and it says so.
// It does not own operating-system signals: a library does not, and a main package
// owns signal.NotifyContext.
func (a *Agent) Run(ctx context.Context, transport Transport) error {
	connection, err := a.Connect(ctx, transport)
	if err != nil {
		return err
	}

	ended := make(chan error, 1)
	go func() { ended <- connection.Wait() }()

	select {
	case err := <-ended:
		return err
	case <-ctx.Done():
		if err := connection.Close(); err != nil {
			return err
		}
		<-ended
		return ctx.Err()
	}
}

func (a *Agent) Conns() iter.Seq[*AgentConn] { return a.conns.all() }

// An AgentConn is one logical connection to a client.
//
// It is not a mirror of [ClientConn]: it serves rather than drives, so it has no
// session-creating methods. Sessions reach an agent through its handlers.
type AgentConn struct {
	connection

	agent    *Agent
	sessions sessions[AgentSession]

	turns        agentTurns
	elicitations urlElicitations
}

// CreateElicitation asks the user for structured input from inside a request this
// connection is serving, for the phases before any session exists.
//
// It must be called from a handler, on the context that handler was given: the
// scope is the request being served and nothing else identifies one. Called
// anywhere else it fails rather than inventing a scope.
func (c *AgentConn) CreateElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
) (*CreateElicitationResponse, error) {
	scope := func(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
		if !params.ToolCallID.IsZero() {
			return nil, errors.New(
				"acp: CreateElicitationParams.ToolCallID names a tool call within a session, " +
					"and a request-scoped elicitation has no session; use AgentSession.CreateElicitation")
		}
		id, method, serving := servingRequest(ctx)
		if !serving {
			return nil, errors.New(
				"acp: AgentConn.CreateElicitation is scoped to the request being served, so it " +
					"must be called from a handler with the context that handler was given")
		}
		// The schema defines this scope as "tied to a specific JSON-RPC request
		// outside of a session". A session-scoped request has a session, so an
		// elicitation from one belongs to that session and AgentSession is where it
		// is asked for; scoping it to the request instead would name a call the
		// client already knows belongs to a conversation.
		if descriptor, standard := standardMethods[method]; standard {
			if descriptor.side != sideAgent {
				return nil, fmt.Errorf(
					"acp: %q is not a request this connection serves, so it cannot be an elicitation's scope",
					method)
			}
			if descriptor.requiresSessionID {
				return nil, fmt.Errorf(
					"acp: %q is scoped to a session because its params require a session ID; "+
						"use AgentSession.CreateElicitation", method)
			}
		}
		within := elicitationRequestScope{RequestID: id}
		switch mode := mode.(type) {
		case *ElicitationFormMode:
			mode.Value = &ElicitationFormModeRequest{elicitationRequestScope: within}
		case *ElicitationURLMode:
			mode.Value = &ElicitationURLModeRequest{elicitationRequestScope: within}
		}
		return mode, nil
	}
	return createElicitation(ctx, c, params, scope)
}

// CompleteElicitation says an accepted URL interaction is finished and the
// client may stop presenting it.
//
// Calling it for an ID whose create response did not accept returns an error.
// When the transport proves a failed send committed no bytes, the ID remains
// outstanding so the caller can retry.
//
// It is on the connection because the notification names only an elicitation, and
// one started before any session exists has no session to send it under.
func (c *AgentConn) CompleteElicitation(
	ctx context.Context,
	params *CompleteElicitationNotification,
) error {
	if params == nil {
		return paramsRequired("CompleteElicitation", "ElicitationID")
	}
	if err := c.awaitHandshake(ctx, methodElicitationComplete); err != nil {
		return err
	}
	if err := c.Peer().permits(methodElicitationComplete); err != nil {
		return err
	}
	completion, outstanding := c.elicitations.beginCompletion(params.ElicitationID)
	if !outstanding {
		return fmt.Errorf(
			"acp: elicitation %q is not outstanding on this connection", params.ElicitationID)
	}
	if err := c.notify(ctx, methodElicitationComplete, params); err != nil {
		// The transport promises that an exact context error means it committed no
		// bytes. writeFailure keeps the connection alive only in that case, so a
		// live connection means retrying this completion is both safe and useful.
		if !c.ended() {
			completion.unsent()
		}
		return err
	}
	completion.sent()
	return nil
}

// Call sends an extension request. Extension methods only; see [ClientConn.Call].
//
// It waits for the handshake. Connect returns before one arrives — this side
// answers a handshake, it does not perform one — so an agent that sent
// immediately would be sending before anybody had agreed what the connection can
// carry. Waiting rather than failing is what makes ctx the caller's answer to
// "how long": an agent with nothing else to do can wait for its client, and one
// that cannot afford to passes a deadline.
func (c *AgentConn) Call(ctx context.Context, method string, params, result any) error {
	if err := c.awaitHandshake(ctx, method); err != nil {
		return err
	}
	return extensionCall(ctx, c.link, method, params, result)
}

// Notify sends an extension notification. Extension methods only, and not before
// the handshake; see [AgentConn.Call].
func (c *AgentConn) Notify(ctx context.Context, method string, params any) error {
	if err := c.awaitHandshake(ctx, method); err != nil {
		return err
	}
	return extensionNotify(ctx, c.link, method, params)
}

// initialize prepares the answer without publishing it.
//
// It does not open the connection. What it negotiated is held until the answer
// has been written, because an agent that started sending on the strength of its
// own decision could put a message ahead of the response that told the client
// what the decision was. The request lifecycle publishes it once the answer is
// on the wire.
//
// The answer is built per connection because a terminal authentication method
// may be advertised only to a client that enabled it.
func (c *AgentConn) initialize(request *InitializeRequest) *InitializeResponse {
	// A protocol version identifies a grammar, so negotiating a numeric minimum
	// could claim a grammar this package does not implement.
	version := CurrentProtocolVersion

	capabilities := deepCopy(c.agent.capabilities)

	response := &InitializeResponse{
		ProtocolVersion:   version,
		AgentCapabilities: capabilities,
		AuthMethods:       c.agent.auth.offered(request.ClientCapabilities),
	}
	if info := c.agent.config.Info; info != nil {
		response.AgentInfo = OptValue(deepCopy(*info))
	}
	if c.agent.config.Meta.Len() > 0 {
		response.Meta = OptValue(deepCopy(c.agent.config.Meta))
	}

	c.handshake.accept(PeerInfo{
		ProtocolVersion:    version,
		ClientCapabilities: deepCopy(request.ClientCapabilities),
		ClientInfo:         request.ClientInfo,
		ClientMeta:         request.Meta,
		AgentCapabilities:  response.AgentCapabilities,
		AgentInfo:          response.AgentInfo,
		AgentMeta:          response.Meta,
		AuthMethods:        response.AuthMethods,
	})
	return response
}

func (c *AgentConn) awaitHandshake(ctx context.Context, method string) error {
	select {
	case <-c.handshake.whenPublished():
		return nil
	case <-c.over():
		return c.failure()
	case <-ctx.Done():
		return fmt.Errorf("acp: method %q waited for the client to initialize: %w", method, ctx.Err())
	}
}

// The same rule the client keeps locally, kept here against any peer: a client
// that does not go through this package can still name a method that was never
// offered, and the handler would otherwise be asked to authenticate something
// this agent never said it could.
func (c *AgentConn) authenticate(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[AuthenticateRequest](request)
	if err != nil {
		return nil, err
	}
	if refusal := c.Peer().authenticates(params.MethodID); refusal != nil {
		return nil, refusal
	}
	if c.agent.config.Authenticate == nil {
		return nil, methodNotImplemented(request.Method)
	}
	response, err := c.agent.config.Authenticate(ctx, c, params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

func (c *AgentConn) serve(ctx context.Context, request *jsonrpc.Request) (any, error) {
	config := &c.agent.config

	if request.Method == methodInitialize {
		params, err := decodeParams[InitializeRequest](request)
		if err != nil {
			return nil, err
		}
		return c.initialize(params), nil
	}

	// The only legal inbound message before initialize is initialize itself.
	// Serving anything else would mean serving it under capabilities nobody has
	// exchanged.
	if !c.handshake.isAccepted() {
		return nil, newError(ErrorCodeInvalidRequest,
			"method %q arrived before initialize", request.Method)
	}

	if isStandardMethod(request.Method) {
		if err := c.Peer().permits(request.Method); err != nil {
			return nil, err
		}
	}

	switch request.Method {
	case methodAuthenticate:
		return c.authenticate(ctx, request)
	case methodSessionNew:
		return c.newSession(ctx, request)
	case methodSessionLoad:
		return dispatchSessionCall(ctx, c, request, config.LoadSession)
	case methodSessionSetMode:
		return dispatchSessionCall(ctx, c, request, config.SetMode)
	case methodSessionSetConfigOption:
		return dispatchSessionCall(ctx, c, request, config.SetConfigOption)
	case methodLogout:
		return dispatchConnCall(ctx, c, request, config.Logout)
	case methodSessionList:
		return dispatchConnCall(ctx, c, request, config.ListSessions)
	case methodSessionDelete:
		return c.deleteSession(ctx, request)
	case methodSessionResume:
		return dispatchSessionCall(ctx, c, request, config.ResumeSession)
	case methodSessionClose:
		return c.closeSession(ctx, request)
	case methodSessionPrompt:
		return c.prompt(ctx, request)
	case methodSessionCancel:
		return nil, c.cancel(ctx, request)
	}

	if isStandardMethod(request.Method) {
		return nil, methodNotImplemented(request.Method)
	}
	return dispatchExtension(ctx, request, config.CallFallback, config.NotifyFallback)
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

// This is where the ordering the peer intended becomes the ordering this
// connection has. A prompt is claimed here rather than in its handler, so a
// cancellation that follows it on the wire finds the turn on record even though
// the handler that serves it may not have started.
//
// Only the claim belongs here. The cancellation itself is an effect on work
// already in progress, and doing it here would let it overtake every message read
// before it — including the responses the peer sent first, which is exactly the
// order a cancelled turn depends on.
func (c *AgentConn) register(request *jsonrpc.Request) registration {
	switch request.Method {
	case methodInitialize:
		// The attempt is queued in wire order, not claimed in a concurrent handler.
		// A later request waits for the preceding answer to settle, then either owns
		// the still-idle negotiation or observes the accepted agreement.
		attempt := c.handshake.registerAttempt()
		return registration{
			dispatch: true,
			admit:    attempt.await,
			settled:  attempt.settle,
			answered: attempt.publish,
		}

	case methodSessionPrompt:
		session, ok := sessionOf(request)
		if !ok {
			// It will fail its own decode in the handler, with a better message.
			return registration{dispatch: true}
		}
		if !c.turns.claim(session, request.ID) {
			// Refused here rather than in the handler, because the handler is not
			// where the ordering is intact. The rule is the one the client handle
			// keeps locally, kept here against any peer.
			refusal := newError(ErrorCodeInvalidRequest,
				"session %s already has a prompt in flight, and session/cancel names no turn", session)
			if c.claimAnswer(request.ID) {
				c.spawn(func() { c.writeResponse(request.ID, nil, refusal) })
			}
			return registration{}
		}
		// Released only after the answer write settles. The peer may send the next
		// prompt after it reads that answer, while one it pipelines before then must
		// still find this turn in flight.
		return registration{
			dispatch: true,
			served:   func() { c.turns.commit(session, request.ID) },
			settled:  func() { c.turns.release(session, request.ID) },
		}

	default:
		return registration{dispatch: true}
	}
}

// The turn was claimed in wire order and is released by the request lifecycle,
// so what is left here is the handler.
func (c *AgentConn) prompt(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[PromptRequest](request)
	if err != nil {
		// Ordered admission may have claimed the session from the small prefix it
		// could decode. Once the full payload is known invalid, commit that failure
		// so a later cancellation cannot reinterpret it as a valid cancelled turn.
		if session, ok := sessionOf(request); ok {
			c.turns.commit(session, request.ID)
		}
		return nil, err
	}
	response, err := c.agent.config.Prompt(ctx, c.session(params.SessionID), params)
	if c.turns.commit(params.SessionID, request.ID) {
		// session/cancel is a semantic turn result, not a failed JSON-RPC call.
		// This boundary catches abort errors from model and tool libraries so they
		// cannot leak as -32603, which the protocol explicitly warns against.
		return &PromptResponse{StopReason: StopReasonCancelled}, nil
	}
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

// This runs from the ordered queue, which is where the cancellation belongs: the
// responses the client sent before it — the cancelled permission outcomes it owed
// the turn — are delivered first, and the agent's own calls return their answers
// rather than the cancellation that was chasing them.
func (c *AgentConn) cancel(ctx context.Context, request *jsonrpc.Request) error {
	params, err := decodeParams[CancelNotification](request)
	if err != nil {
		return err
	}
	c.cancelTurn(params.SessionID)
	c.agent.config.Cancel(ctx, c.session(params.SessionID), params)
	return nil
}

// It does not answer the prompt. The protocol says what the answer is — the
// cancelled stop reason — and it is the agent's handler that owes it: the client
// is still waiting on that response, and the agent may have final tool-call
// updates to send before it. Other sessions and unrelated calls are untouched.
//
// The turn's handler may not have started yet, and that is not a problem the turn
// has to solve: the request's context is created when the request is read, so
// cancelling it now means the handler starts already cancelled.
func (c *AgentConn) cancelTurn(session SessionID) {
	id, cancellable := c.turns.cancel(session)
	if !cancellable {
		// Nothing to interrupt: either no turn is on record, or its handler already
		// committed the result. The turn is claimed in wire order, so a prompt this
		// cancellation follows cannot be missing merely because its handler has not
		// started.
		return
	}
	c.interruptRequest(id)
}
