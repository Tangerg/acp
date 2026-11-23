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

// AgentConfig defines an agent's identity, handlers and capability
// advertisement. NewSession and Prompt are required. Setting an optional handler
// advertises the capability that gates its method, unless Capabilities states
// the advertisement whole.
//
// A handler is given the handle its method is scoped to: a session-scoped method
// gets an [AgentSession], a connection-scoped one the [AgentConn] the request
// arrived on, which is how a handler with no session can still call the client,
// for example to elicit input during authentication.
type AgentConfig struct {
	// Info identifies this agent during initialize.
	Info *Implementation
	// Meta is attached to this agent's initialize response.
	Meta Meta
	// Logger receives handler failures, which the peer is not shown. nil
	// discards them.
	Logger *slog.Logger

	// AuthMethods is what this agent offers a client that must authenticate. An
	// agent-handled method requires Authenticate.
	AuthMethods []AuthMethod

	NewSession func(ctx context.Context, conn *AgentConn, request *NewSessionRequest) (*NewSessionResponse, error)
	Prompt     func(ctx context.Context, session *AgentSession, request *PromptRequest) (*PromptResponse, error)

	// Cancel is told that the client cancelled a session's turn. The connection
	// has already cancelled the turn's context and answers the prompt with the
	// cancelled stop reason whatever its handler returns, so most agents leave
	// this nil.
	Cancel func(ctx context.Context, session *AgentSession, notification *CancelNotification)

	// Authenticate performs an agent-handled method listed in AuthMethods. The
	// identifier has already been checked against what this agent offered.
	Authenticate func(ctx context.Context, conn *AgentConn, request *AuthenticateRequest) (*AuthenticateResponse, error)

	// LoadSession advertises loadSession.
	LoadSession func(ctx context.Context, session *AgentSession, request *LoadSessionRequest) (*LoadSessionResponse, error)

	// SetMode and SetConfigOption are gated by data rather than capability: an
	// agent offers modes and options by returning them from session/new.
	SetMode         func(ctx context.Context, session *AgentSession, request *SetSessionModeRequest) (*SetSessionModeResponse, error)
	SetConfigOption func(
		ctx context.Context,
		session *AgentSession,
		request *SetSessionConfigOptionRequest,
	) (*SetSessionConfigOptionResponse, error)

	// Logout advertises auth.logout.
	Logout func(ctx context.Context, conn *AgentConn, request *LogoutRequest) (*LogoutResponse, error)

	// The session lifecycle, each advertising its sessionCapabilities property.
	// CloseSession runs after the connection has cancelled the session's turn.
	ListSessions  func(ctx context.Context, conn *AgentConn, request *ListSessionsRequest) (*ListSessionsResponse, error)
	DeleteSession func(ctx context.Context, session *AgentSession, request *DeleteSessionRequest) (*DeleteSessionResponse, error)
	ResumeSession func(ctx context.Context, session *AgentSession, request *ResumeSessionRequest) (*ResumeSessionResponse, error)
	CloseSession  func(ctx context.Context, session *AgentSession, request *CloseSessionRequest) (*CloseSessionResponse, error)

	// CallFallback and NotifyFallback receive extension methods: names beginning
	// with an underscore. A nil result answers with an empty object.
	CallFallback   func(ctx context.Context, conn *AgentConn, request *ExtRequest) (any, error)
	NotifyFallback func(ctx context.Context, conn *AgentConn, notification *ExtNotification)

	// Capabilities, when set, is the complete advertisement rather than a patch.
	// Construction fails if it claims a method no handler serves.
	Capabilities *AgentCapabilities

	// Limits bounds the protocol state each connection holds.
	Limits Limits
}

// An Agent owns the handlers and capabilities its connections share. One agent
// may serve many clients.
type Agent struct {
	config       AgentConfig
	capabilities AgentCapabilities
	auth         authenticationMethods
	conns        registry[*AgentConn]
}

// NewAgent builds an agent, or reports why the configuration cannot be served.
func NewAgent(config *AgentConfig) (*Agent, error) {
	if config == nil {
		config = new(AgentConfig)
	}
	if config.NewSession == nil || config.Prompt == nil {
		return nil, errors.New("acp: AgentConfig requires NewSession and Prompt")
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
	cloned := *config
	cloned.Info = deepCopy(config.Info)
	cloned.Meta = deepCopy(config.Meta)
	cloned.AuthMethods = nil
	cloned.Capabilities = nil
	return &Agent{config: cloned, capabilities: capabilities, auth: auth}, nil
}

// serves reports whether the configuration serves a standard method.
func (a *AgentConfig) serves(method string) bool {
	if binding, ok := agentMethods[method]; ok {
		return binding.served(a)
	}
	return method == methodSessionCancel
}

func (a *AgentConfig) resolveCapabilities() (AgentCapabilities, error) {
	if a.Capabilities == nil {
		return advertiseAgent(a.serves), nil
	}
	stated := deepCopy(*a.Capabilities)
	if unserved := unservedAgentMethods(stated, a.serves); len(unserved) > 0 {
		return AgentCapabilities{}, fmt.Errorf("acp: AgentConfig.Capabilities: %v", unserved)
	}
	return stated, nil
}

// Connect accepts a connection and returns once its read loop runs, before the
// client sends initialize: an agent answers a handshake rather than performing
// one. Until initialize arrives every other request is answered -32600.
//
// ctx scopes setup only; [AgentConn.Close] owns the connection's lifetime.
func (a *Agent) Connect(ctx context.Context, transport Transport) (*AgentConn, error) {
	stream, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	conn := newAgentConn(a, stream)
	conn.run()
	a.conns.add(conn, conn.life)
	return conn, nil
}

// newAgentConn assembles a connection whose read loop has not started.
func newAgentConn(agent *Agent, stream Connection) *AgentConn {
	conn := &AgentConn{agent: agent}
	conn.connection = connection{negotiation: newNegotiation()}
	conn.link = newLink(stream, conn, agent.config.Logger, agent.config.Limits)
	conn.elicitations = newURLElicitations(conn.limits.OutstandingElicitations)
	return conn
}

// Run serves one connection until ctx is cancelled or the connection ends, then
// closes it and waits for everything it owns to stop. It does not own operating
// system signals; a main package owns signal.NotifyContext.
func (a *Agent) Run(ctx context.Context, transport Transport) error {
	conn, err := a.Connect(ctx, transport)
	if err != nil {
		return err
	}
	ended := make(chan error, 1)
	go func() { ended <- conn.Wait() }()

	select {
	case endErr := <-ended:
		return endErr
	case <-ctx.Done():
		closeErr := conn.Close()
		endErr := <-ended
		return errors.Join(ctx.Err(), closeErr, endErr)
	}
}

// Conns is a snapshot of the live connections, each registered when accepted.
func (a *Agent) Conns() iter.Seq[*AgentConn] { return a.conns.all() }

// An AgentConn is one logical connection to a client. It serves rather than
// drives, so sessions reach an agent through its handlers.
type AgentConn struct {
	connection

	agent        *Agent
	turns        agentTurns
	elicitations *urlElicitations
}

// session returns the handle for a session on this connection.
func (c *AgentConn) session(id SessionID) *AgentSession {
	return &AgentSession{id: id, conn: c}
}

// CreateElicitation asks the user for structured input from inside a request
// this connection is serving, for the phases before a session exists. It must be
// called with the context the handler was given, because the request being
// served is the scope.
func (c *AgentConn) CreateElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
) (*CreateElicitationResponse, error) {
	call, serving := servingRequest(ctx)
	if !serving || call.owner != c.requests {
		return nil, errors.New("acp: AgentConn.CreateElicitation must be called from a handler, with the " +
			"context that handler was given")
	}
	if params != nil && !params.ToolCallID.IsZero() {
		return nil, errors.New("acp: CreateElicitationParams.ToolCallID names a tool call within a session, " +
			"and a request-scoped elicitation has no session; use AgentSession.CreateElicitation")
	}
	// The schema defines this scope as tied to a request outside a session, so a
	// session-scoped request elicits through its session instead.
	if descriptor, standard := standardMethods[call.method]; standard && descriptor.requiresSessionID {
		return nil, fmt.Errorf("acp: %q is scoped to a session; use AgentSession.CreateElicitation", call.method)
	}
	return c.createElicitation(ctx, params, requestScope{request: call.id}, call)
}

// CompleteElicitation says an accepted URL interaction is finished. It fails for
// an ID whose create response did not accept. When the transport proves a failed
// send committed nothing, the ID stays outstanding so the caller can retry.
func (c *AgentConn) CompleteElicitation(ctx context.Context, params *CompleteElicitationNotification) error {
	if params == nil {
		params = new(CompleteElicitationNotification)
	}
	if err := c.checkOutbound(ctx, methodElicitationComplete, params); err != nil {
		return err
	}
	completion, outstanding := c.elicitations.beginCompletion(params.ElicitationID)
	if !outstanding {
		return fmt.Errorf("acp: elicitation %q is not outstanding on this connection", params.ElicitationID)
	}
	if err := c.notify(ctx, methodElicitationComplete, params); err != nil {
		if !c.life.ended() {
			completion.unsent()
		}
		return err
	}
	completion.sent()
	return nil
}

// Call sends an extension request and decodes its result. It waits for the
// client's initialize, because nothing may be sent before the agreement is.
func (c *AgentConn) Call(ctx context.Context, method string, params, result any) error {
	return c.extensionCall(ctx, method, params, result)
}

// Notify sends an extension notification once the client has initialized.
func (c *AgentConn) Notify(ctx context.Context, method string, params any) error {
	return c.extensionNotify(ctx, method, params)
}

// An agentMethod is how an agent serves one standard method.
type agentMethod struct {
	served func(*AgentConfig) bool
	decode func(json.RawMessage) (any, error)
	// register, when set, records the call in wire order.
	register func(*AgentConn, *inboundRequest) registration
	serve    func(*AgentConn, context.Context, *inboundRequest) (any, error)
}

func always(*AgentConfig) bool { return true }

func agentConnMethod[Request, Response any](
	pick func(*AgentConfig) func(context.Context, *AgentConn, *Request) (*Response, error),
) agentMethod {
	return agentMethod{
		served: func(config *AgentConfig) bool { return pick(config) != nil },
		decode: decoderFor[Request],
		serve: func(c *AgentConn, ctx context.Context, call *inboundRequest) (any, error) {
			response, err := pick(&c.agent.config)(ctx, c, paramsOf[Request](call))
			return handlerAnswer(call.method, response, err)
		},
	}
}

func agentSessionMethod[Request any, Scoped interface {
	*Request
	sessionScoped
}, Response any](
	pick func(*AgentConfig) func(context.Context, *AgentSession, Scoped) (*Response, error),
) agentMethod {
	return agentMethod{
		served: func(config *AgentConfig) bool { return pick(config) != nil },
		decode: decoderFor[Request],
		serve: func(c *AgentConn, ctx context.Context, call *inboundRequest) (any, error) {
			request := Scoped(paramsOf[Request](call))
			response, err := pick(&c.agent.config)(ctx, c.session(request.sessionID()), request)
			return handlerAnswer(call.method, response, err)
		},
	}
}

var agentMethods = map[string]agentMethod{
	methodInitialize: {
		served:   always,
		decode:   decoderFor[InitializeRequest],
		register: (*AgentConn).registerInitialize,
	},
	methodAuthenticate: {
		served: func(config *AgentConfig) bool { return config.Authenticate != nil },
		decode: decoderFor[AuthenticateRequest],
		serve:  (*AgentConn).serveAuthenticate,
	},
	methodSessionNew: agentConnMethod(func(config *AgentConfig) func(
		context.Context, *AgentConn, *NewSessionRequest) (*NewSessionResponse, error) {
		return config.NewSession
	}),
	methodLogout: agentConnMethod(func(config *AgentConfig) func(
		context.Context, *AgentConn, *LogoutRequest) (*LogoutResponse, error) {
		return config.Logout
	}),
	methodSessionList: agentConnMethod(func(config *AgentConfig) func(
		context.Context, *AgentConn, *ListSessionsRequest) (*ListSessionsResponse, error) {
		return config.ListSessions
	}),
	methodSessionLoad: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *LoadSessionRequest) (*LoadSessionResponse, error) {
		return config.LoadSession
	}),
	methodSessionResume: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *ResumeSessionRequest) (*ResumeSessionResponse, error) {
		return config.ResumeSession
	}),
	methodSessionSetMode: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *SetSessionModeRequest) (*SetSessionModeResponse, error) {
		return config.SetMode
	}),
	methodSessionSetConfigOption: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *SetSessionConfigOptionRequest) (*SetSessionConfigOptionResponse, error) {
		return config.SetConfigOption
	}),
	methodSessionDelete: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *DeleteSessionRequest) (*DeleteSessionResponse, error) {
		return config.DeleteSession
	}),
	methodSessionClose: agentSessionMethod(func(config *AgentConfig) func(
		context.Context, *AgentSession, *CloseSessionRequest) (*CloseSessionResponse, error) {
		if config.CloseSession == nil {
			return nil
		}
		return closeSession(config.CloseSession)
	}),
	methodSessionPrompt: {
		served:   always,
		decode:   decoderFor[PromptRequest],
		register: (*AgentConn).registerPrompt,
		serve:    (*AgentConn).servePrompt,
	},
}

// closeSession cancels the session's turn before the handler frees it: the
// schema says the agent "must cancel any ongoing work related to the session
// (treat it as if session/cancel was called)", and the turn is the connection's.
// The application's Cancel handler is not invoked; it is about to be told
// something more specific.
func closeSession(
	handler func(context.Context, *AgentSession, *CloseSessionRequest) (*CloseSessionResponse, error),
) func(context.Context, *AgentSession, *CloseSessionRequest) (*CloseSessionResponse, error) {
	return func(ctx context.Context, session *AgentSession, request *CloseSessionRequest) (*CloseSessionResponse, error) {
		session.conn.cancelTurn(session.id)
		return handler(ctx, session, request)
	}
}

// register decides a call in wire order: initialize before anything else, the
// capability gate, the method's handler, and its params, decoded once here.
func (c *AgentConn) register(call *inboundRequest, request *jsonrpc.Request) registration {
	binding, served := agentMethods[request.Method]
	if request.Method != methodInitialize {
		agreed := c.negotiation.agreed()
		if agreed == nil {
			return refuseBeforeInitialize(request.Method)
		}
		if isStandardMethod(request.Method) {
			if refusal := agreed.refusesInbound(request.Method); refusal != nil {
				return answered(nil, refusal)
			}
		}
	}
	if !served {
		if isStandardMethod(request.Method) {
			return answered(nil, methodNotImplemented(request.Method))
		}
		call.params = request.Params
		return dispatched(callHooks{})
	}
	if !binding.served(&c.agent.config) {
		return answered(nil, methodNotImplemented(request.Method))
	}
	params, err := binding.decode(request.Params)
	if err != nil {
		return answered(nil, invalidParams(request.Method, err))
	}
	call.params = params
	if binding.register != nil {
		return binding.register(c, call)
	}
	return dispatched(callHooks{})
}

// serve runs a dispatched call and holds its response to the agreement, which
// covers responses whose caller has already stopped waiting.
func (c *AgentConn) serve(ctx context.Context, call *inboundRequest) (any, error) {
	binding, standard := agentMethods[call.method]
	if !standard {
		return serveExtensionCall(ctx, c, call, c.agent.config.CallFallback)
	}
	response, err := binding.serve(c, ctx, call)
	if err != nil {
		return nil, err
	}
	if checked, ok := response.(outboundChecked); ok {
		if invalid := checked.checkOutbound(c.negotiation.agreed()); invalid != nil {
			c.logger.Error("a handler's response cannot be sent under the negotiated capabilities",
				slog.String("method", call.method), slog.Any("error", invalid))
			return nil, newError(ErrorCodeInternalError, "handler for method %q returned a response the "+
				"negotiated capabilities do not permit", call.method)
		}
	}
	return response, nil
}

func (c *AgentConn) serveNotification(ctx context.Context, request *jsonrpc.Request) error {
	if c.negotiation.agreed() == nil {
		return fmt.Errorf("acp: notification %q arrived before initialize", request.Method)
	}
	switch request.Method {
	case methodSessionCancel:
		params, err := decodeParams[CancelNotification](request.Params)
		if err != nil {
			return invalidParams(request.Method, err)
		}
		c.serveCancel(ctx, params)
		return nil
	default:
		if isStandardMethod(request.Method) {
			return methodNotImplemented(request.Method)
		}
		serveExtensionNotification(ctx, c, request.Method, request.Params, c.agent.config.NotifyFallback)
		return nil
	}
}

// registerInitialize negotiates in wire order, so every message read after an
// initialize is judged against what it settled. The agreement is accepted before
// the answer is written but opened for this side's own sends only once the
// answer is on the wire. A later initialize renegotiates.
func (c *AgentConn) registerInitialize(call *inboundRequest) registration {
	request := paramsOf[InitializeRequest](call)
	agent := c.agent
	response := &InitializeResponse{
		// A protocol version names a grammar, so this side answers the one it
		// implements rather than a negotiated minimum.
		ProtocolVersion:   CurrentProtocolVersion,
		AgentCapabilities: deepCopy(agent.capabilities),
		AuthMethods:       agent.auth.offeredTo(request.ClientCapabilities),
	}
	if info := agent.config.Info; info != nil {
		response.AgentInfo = OptValue(deepCopy(*info))
	}
	if agent.config.Meta.Len() > 0 {
		response.Meta = OptValue(deepCopy(agent.config.Meta))
	}
	c.negotiation.accept(Negotiated{
		ClientCapabilities: request.ClientCapabilities,
		ClientInfo:         request.ClientInfo,
		ClientMeta:         request.Meta,
		AgentCapabilities:  response.AgentCapabilities,
		AgentInfo:          response.AgentInfo,
		AgentMeta:          response.Meta,
		AuthMethods:        response.AuthMethods,
	})
	return answeredThen(response, func(written any) {
		if written != nil {
			c.negotiation.open()
		}
	})
}

// serveAuthenticate holds the identifier to what this agent offered before its
// handler is asked to authenticate it.
func (c *AgentConn) serveAuthenticate(ctx context.Context, call *inboundRequest) (any, error) {
	request := paramsOf[AuthenticateRequest](call)
	if refusal := c.negotiation.agreed().authenticates(request.MethodID); refusal != nil {
		return nil, refusal
	}
	response, err := c.agent.config.Authenticate(ctx, c, request)
	return handlerAnswer(call.method, response, err)
}

// registerPrompt claims the session's turn in wire order, so a cancellation that
// follows the prompt on the wire finds the turn even before its handler starts.
// The turn is committed when its answer is chosen, because the peer can receive
// the answer and send its next prompt before the write returns.
func (c *AgentConn) registerPrompt(call *inboundRequest) registration {
	session := paramsOf[PromptRequest](call).SessionID
	if !c.turns.claim(session, call) {
		return answered(nil, newError(ErrorCodeInvalidRequest,
			"session %s already has a prompt in flight, and session/cancel names no turn", session))
	}
	return dispatched(callHooks{
		chosen:   func(any, error) { c.turns.commit(session, call) },
		finished: func(any) { c.turns.release(session, call) },
	})
}

// servePrompt answers a cancelled turn with the cancelled stop reason whatever
// its handler returned: model and tool libraries abort with their context's
// error, and the protocol warns against relaying that as -32603.
func (c *AgentConn) servePrompt(ctx context.Context, call *inboundRequest) (any, error) {
	request := paramsOf[PromptRequest](call)
	// The turn is the one registration claimed: a handler that edits its request
	// cannot redirect the commit to another session.
	session := request.SessionID
	response, err := c.agent.config.Prompt(ctx, c.session(session), request)
	if c.turns.commit(session, call) {
		if response == nil {
			response = new(PromptResponse)
		}
		response.StopReason = StopReasonCancelled
		return response, nil
	}
	return handlerAnswer(call.method, response, err)
}

// serveCancel runs in wire order, behind the responses the client sent before it,
// so the agent's own calls return their answers rather than the cancellation.
func (c *AgentConn) serveCancel(ctx context.Context, notification *CancelNotification) {
	c.cancelTurn(notification.SessionID)
	if handler := c.agent.config.Cancel; handler != nil {
		handler(ctx, c.session(notification.SessionID), notification)
	}
}

// cancelTurn cancels the context of a session's uncommitted turn. A handler that
// has not started yet starts already cancelled. It does not answer the prompt:
// the handler owes that, after any final updates.
func (c *AgentConn) cancelTurn(session SessionID) {
	if call, cancellable := c.turns.cancel(session); cancellable {
		call.cancel()
	}
}
