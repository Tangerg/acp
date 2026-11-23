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

// ClientConfig defines a client's identity, handlers and capability
// advertisement. SessionUpdate and RequestPermission are required. Setting an
// optional handler advertises the capability that gates its method, unless
// Capabilities states the advertisement whole.
//
// Every handler receives the connection a request arrived on: one client may
// drive several agents, and their session, terminal and request identifiers may
// coincide.
type ClientConfig struct {
	// Info identifies this client during initialize.
	Info *Implementation
	// Meta is attached to this client's initialize request.
	Meta Meta
	// Logger receives handler failures, which the peer is not shown, and
	// notification failures, which have no response to carry them. nil discards
	// them.
	Logger *slog.Logger

	SessionUpdate func(ctx context.Context, conn *ClientConn, notification *SessionNotification)

	// RequestPermission is required because there is no safe default: an agent
	// need not offer a reject option, so there is no universal deny to invent.
	RequestPermission func(ctx context.Context, conn *ClientConn, request *RequestPermissionRequest) (*RequestPermissionResponse, error)

	// ReadTextFile and WriteTextFile are gated on two independent capabilities.
	ReadTextFile  func(ctx context.Context, conn *ClientConn, request *ReadTextFileRequest) (*ReadTextFileResponse, error)
	WriteTextFile func(ctx context.Context, conn *ClientConn, request *WriteTextFileRequest) (*WriteTextFileResponse, error)

	// Terminal is all five terminal methods or none, because one capability
	// gates all five.
	Terminal *TerminalHandlers

	// Elicitation advertises each mode it renders.
	Elicitation *ElicitationHandlers

	// CallFallback and NotifyFallback receive extension methods: names beginning
	// with an underscore. A nil result answers with an empty object.
	CallFallback   func(ctx context.Context, conn *ClientConn, request *ExtRequest) (any, error)
	NotifyFallback func(ctx context.Context, conn *ClientConn, notification *ExtNotification)

	// Capabilities, when set, is the complete advertisement rather than a patch,
	// because a boolean has no third state to say "not set". Construction fails if
	// it claims anything the handlers do not serve.
	Capabilities *ClientCapabilities

	// Limits bounds the protocol state each connection holds.
	Limits Limits
}

// TerminalHandlers is the complete terminal capability: all five handlers are
// required, because one capability advertises all five methods.
type TerminalHandlers struct {
	Create      func(ctx context.Context, conn *ClientConn, request *CreateTerminalRequest) (*CreateTerminalResponse, error)
	Output      func(ctx context.Context, conn *ClientConn, request *TerminalOutputRequest) (*TerminalOutputResponse, error)
	WaitForExit func(ctx context.Context, conn *ClientConn, request *WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error)
	Kill        func(ctx context.Context, conn *ClientConn, request *KillTerminalRequest) (*KillTerminalResponse, error)
	Release     func(ctx context.Context, conn *ClientConn, request *ReleaseTerminalRequest) (*ReleaseTerminalResponse, error)
}

// check refuses a partial set, which would refuse a method it advertised.
func (t *TerminalHandlers) check() error {
	if t == nil {
		return nil
	}
	var missing []string
	for _, handler := range []struct {
		name string
		set  bool
	}{
		{"Create", t.Create != nil},
		{"Output", t.Output != nil},
		{"WaitForExit", t.WaitForExit != nil},
		{"Kill", t.Kill != nil},
		{"Release", t.Release != nil},
	} {
		if !handler.set {
			missing = append(missing, handler.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("acp: TerminalHandlers is incomplete; missing: %v", missing)
	}
	return nil
}

// A Client owns the handlers and capabilities its connections share. One client
// may connect to many agents.
type Client struct {
	config       ClientConfig
	capabilities ClientCapabilities
	conns        registry[*ClientConn]
}

// NewClient builds a client, or reports why the configuration cannot be served.
func NewClient(config *ClientConfig) (*Client, error) {
	if config == nil {
		config = new(ClientConfig)
	}
	if config.SessionUpdate == nil || config.RequestPermission == nil {
		return nil, errors.New("acp: ClientConfig requires SessionUpdate and RequestPermission")
	}
	if err := config.Terminal.check(); err != nil {
		return nil, err
	}
	if err := config.Elicitation.check(); err != nil {
		return nil, err
	}
	if err := config.Limits.check(); err != nil {
		return nil, err
	}
	capabilities, err := config.resolveCapabilities()
	if err != nil {
		return nil, err
	}
	// The configuration is copied so a caller who kept a pointer cannot change
	// what a running client advertises or serves without going back through the
	// checks above.
	cloned := *config
	cloned.Info = deepCopy(config.Info)
	cloned.Meta = deepCopy(config.Meta)
	cloned.Capabilities = nil
	if config.Terminal != nil {
		handlers := *config.Terminal
		cloned.Terminal = &handlers
	}
	if config.Elicitation != nil {
		handlers := *config.Elicitation
		cloned.Elicitation = &handlers
	}
	return &Client{config: cloned, capabilities: capabilities}, nil
}

// serves reports whether the configuration serves a standard method.
func (c *ClientConfig) serves(method string) bool {
	switch method {
	case methodSessionUpdate:
		return true
	case methodElicitationComplete:
		return c.Elicitation != nil && c.Elicitation.URL != nil
	}
	binding, ok := clientMethods[method]
	return ok && binding.served(c)
}

func (c *ClientConfig) resolveCapabilities() (ClientCapabilities, error) {
	if c.Capabilities == nil {
		derived := advertiseClient(c.serves)
		derived.Elicitation = c.Elicitation.capabilities()
		return derived, nil
	}
	stated := deepCopy(*c.Capabilities)
	unserved := append(unservedClientMethods(stated, c.serves), c.Elicitation.unserved(stated)...)
	if len(unserved) > 0 {
		return ClientCapabilities{}, fmt.Errorf("acp: ClientConfig.Capabilities: %v", unserved)
	}
	return stated, nil
}

// Connect performs the handshake and returns a connection already initialized:
// the transport connected, initialize answered with the protocol version this
// package implements, and the agent's capabilities stored. If any of that fails
// the connection is closed before Connect returns.
//
// ctx scopes setup only; [ClientConn.Close] owns the connection's lifetime.
func (c *Client) Connect(ctx context.Context, transport Transport) (*ClientConn, error) {
	stream, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	conn := newClientConn(c, stream)
	conn.run()

	if initErr := conn.initialize(ctx); initErr != nil {
		_ = conn.Close()
		return nil, initErr
	}
	c.conns.add(conn, conn.life)
	return conn, nil
}

// newClientConn assembles a connection whose read loop has not started.
func newClientConn(client *Client, stream Connection) *ClientConn {
	conn := &ClientConn{client: client}
	conn.connection = connection{negotiation: newNegotiation()}
	conn.link = newLink(stream, conn, client.config.Logger, client.config.Limits)
	conn.elicitations = newURLElicitations(conn.limits.OutstandingElicitations)
	return conn
}

// Conns is a snapshot of the live connections, each registered once its
// handshake succeeded.
func (c *Client) Conns() iter.Seq[*ClientConn] { return c.conns.all() }

// A ClientConn is one logical connection to an agent, after initialize. It is
// named for the connection because the specification already means something by
// "session": a conversation, of which one connection carries many.
type ClientConn struct {
	connection

	client       *Client
	turns        clientTurns
	elicitations *urlElicitations
}

func (c *ClientConn) session(id SessionID) *ClientSession {
	return &ClientSession{id: id, conn: c}
}

// Authenticate performs one of the agent-handled authentication methods in
// [ClientConn.Negotiated], which is how a client answers [ErrAuthRequired].
func (c *ClientConn) Authenticate(ctx context.Context, params *AuthenticateRequest) (*AuthenticateResponse, error) {
	if params == nil {
		params = new(AuthenticateRequest)
	}
	if refusal := c.negotiation.agreed().authenticates(params.MethodID); refusal != nil {
		return nil, refusal
	}
	return callFor[AuthenticateResponse](ctx, &c.connection, methodAuthenticate, params)
}

// Logout ends the authentication a client established, without ending the
// connection.
//
// Gated on agentCapabilities.auth.logout.
func (c *ClientConn) Logout(ctx context.Context, params *LogoutRequest) (*LogoutResponse, error) {
	if params == nil {
		params = new(LogoutRequest)
	}
	return callFor[LogoutResponse](ctx, &c.connection, methodLogout, params)
}

// NewSession opens a conversation and returns its handle with the response,
// which carries modes, configuration options and _meta. An agent that requires
// authentication answers with [ErrAuthRequired]: authenticate, then retry.
func (c *ClientConn) NewSession(ctx context.Context, params *NewSessionRequest) (*ClientSession, *NewSessionResponse, error) {
	if params == nil {
		params = new(NewSessionRequest)
	}
	response, err := callFor[NewSessionResponse](ctx, &c.connection, methodSessionNew, params)
	if err != nil {
		return nil, nil, err
	}
	return c.session(response.SessionID), response, nil
}

// LoadSession reopens a conversation the agent already has, replaying its
// history as [SessionNotification] updates before it answers.
//
// Gated on agentCapabilities.loadSession.
func (c *ClientConn) LoadSession(ctx context.Context, params *LoadSessionRequest) (*ClientSession, *LoadSessionResponse, error) {
	if params == nil {
		params = new(LoadSessionRequest)
	}
	response, err := callFor[LoadSessionResponse](ctx, &c.connection, methodSessionLoad, params)
	if err != nil {
		return nil, nil, err
	}
	return c.session(params.SessionID), response, nil
}

// ResumeSession reopens a session the agent still has, without replaying it.
//
// Gated on agentCapabilities.sessionCapabilities.resume.
func (c *ClientConn) ResumeSession(
	ctx context.Context,
	params *ResumeSessionRequest,
) (*ClientSession, *ResumeSessionResponse, error) {
	if params == nil {
		params = new(ResumeSessionRequest)
	}
	response, err := callFor[ResumeSessionResponse](ctx, &c.connection, methodSessionResume, params)
	if err != nil {
		return nil, nil, err
	}
	return c.session(params.SessionID), response, nil
}

// ListSessions reports one page of the sessions the agent has stored. A response
// with NextCursor has more behind it.
//
// Gated on agentCapabilities.sessionCapabilities.list.
func (c *ClientConn) ListSessions(ctx context.Context, params *ListSessionsRequest) (*ListSessionsResponse, error) {
	if params == nil {
		params = new(ListSessionsRequest)
	}
	return callFor[ListSessionsResponse](ctx, &c.connection, methodSessionList, params)
}

// DeleteSession removes a stored session, which may be one this connection
// never opened. An agent answering resource-not-found has no such session, which
// leaves the same end state, so that is a successful delete.
//
// Gated on agentCapabilities.sessionCapabilities.delete.
func (c *ClientConn) DeleteSession(ctx context.Context, params *DeleteSessionRequest) (*DeleteSessionResponse, error) {
	if params == nil {
		params = new(DeleteSessionRequest)
	}
	response, err := callFor[DeleteSessionResponse](ctx, &c.connection, methodSessionDelete, params)
	if errors.Is(err, &Error{Code: ErrorCodeResourceNotFound}) {
		return new(DeleteSessionResponse), nil
	}
	return response, err
}

// Call sends an extension request and decodes its result. Extension methods
// only: a standard method has one path, through its operation.
func (c *ClientConn) Call(ctx context.Context, method string, params, result any) error {
	return c.extensionCall(ctx, method, params, result)
}

// Notify sends an extension notification.
func (c *ClientConn) Notify(ctx context.Context, method string, params any) error {
	return c.extensionNotify(ctx, method, params)
}

// initialize negotiates on the delivery loop, so that anything the agent sends
// after answering is served by a connection that already knows what was agreed.
func (c *ClientConn) initialize(ctx context.Context) error {
	request := &InitializeRequest{
		ProtocolVersion:    CurrentProtocolVersion,
		ClientCapabilities: c.client.capabilities,
	}
	if info := c.client.config.Info; info != nil {
		request.ClientInfo = OptValue(deepCopy(*info))
	}
	if c.client.config.Meta.Len() > 0 {
		request.Meta = OptValue(deepCopy(c.client.config.Meta))
	}
	return c.callObserved(ctx, methodInitialize, request, func(answer *jsonrpc.Response) error {
		var response InitializeResponse
		if err := decodeResponse(answer, &response); err != nil {
			return err
		}
		// A protocol version names a grammar; any other answer claims one this
		// package does not implement.
		if response.ProtocolVersion != CurrentProtocolVersion {
			return fmt.Errorf("acp: the agent answered initialize with protocol version %d, and this package "+
				"implements %d and no other", response.ProtocolVersion, CurrentProtocolVersion)
		}
		c.negotiation.accept(Negotiated{
			ClientCapabilities: request.ClientCapabilities,
			ClientInfo:         request.ClientInfo,
			ClientMeta:         request.Meta,
			AgentCapabilities:  response.AgentCapabilities,
			AgentInfo:          response.AgentInfo,
			AgentMeta:          response.Meta,
			AuthMethods:        receivedAuthenticationMethods(response.AuthMethods, request.ClientCapabilities, c.logger),
		})
		c.negotiation.open()
		return nil
	})
}

// A clientMethod is how a client serves one standard request method.
type clientMethod struct {
	served func(*ClientConfig) bool
	decode func(json.RawMessage) (any, error)
	// register, when set, records the call in wire order.
	register func(*ClientConn, *inboundRequest) registration
	serve    func(*ClientConn, context.Context, *inboundRequest) (any, error)
}

func clientConnMethod[Request, Response any](
	pick func(*ClientConfig) func(context.Context, *ClientConn, *Request) (*Response, error),
) clientMethod {
	return clientMethod{
		served: func(config *ClientConfig) bool { return pick(config) != nil },
		decode: decoderFor[Request],
		serve: func(c *ClientConn, ctx context.Context, call *inboundRequest) (any, error) {
			response, err := pick(&c.client.config)(ctx, c, paramsOf[Request](call))
			return handlerAnswer(call.method, response, err)
		},
	}
}

// terminalMethod picks one handler of the terminal group.
func terminalMethod[Request, Response any](
	pick func(*TerminalHandlers) func(context.Context, *ClientConn, *Request) (*Response, error),
) clientMethod {
	return clientConnMethod(func(config *ClientConfig) func(context.Context, *ClientConn, *Request) (*Response, error) {
		if config.Terminal == nil {
			return nil
		}
		return pick(config.Terminal)
	})
}

var clientMethods = map[string]clientMethod{
	methodSessionRequestPermission: {
		served:   func(*ClientConfig) bool { return true },
		decode:   decoderFor[RequestPermissionRequest],
		register: (*ClientConn).registerPermission,
		serve:    (*ClientConn).servePermission,
	},
	methodFsReadTextFile: clientConnMethod(func(config *ClientConfig) func(
		context.Context, *ClientConn, *ReadTextFileRequest) (*ReadTextFileResponse, error) {
		return config.ReadTextFile
	}),
	methodFsWriteTextFile: clientConnMethod(func(config *ClientConfig) func(
		context.Context, *ClientConn, *WriteTextFileRequest) (*WriteTextFileResponse, error) {
		return config.WriteTextFile
	}),
	methodTerminalCreate: terminalMethod(func(handlers *TerminalHandlers) func(
		context.Context, *ClientConn, *CreateTerminalRequest) (*CreateTerminalResponse, error) {
		return handlers.Create
	}),
	methodTerminalOutput: terminalMethod(func(handlers *TerminalHandlers) func(
		context.Context, *ClientConn, *TerminalOutputRequest) (*TerminalOutputResponse, error) {
		return handlers.Output
	}),
	methodTerminalWaitForExit: terminalMethod(func(handlers *TerminalHandlers) func(
		context.Context, *ClientConn, *WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error) {
		return handlers.WaitForExit
	}),
	methodTerminalKill: terminalMethod(func(handlers *TerminalHandlers) func(
		context.Context, *ClientConn, *KillTerminalRequest) (*KillTerminalResponse, error) {
		return handlers.Kill
	}),
	methodTerminalRelease: terminalMethod(func(handlers *TerminalHandlers) func(
		context.Context, *ClientConn, *ReleaseTerminalRequest) (*ReleaseTerminalResponse, error) {
		return handlers.Release
	}),
	methodElicitationCreate: {
		served:   func(config *ClientConfig) bool { return config.Elicitation != nil },
		decode:   decoderFor[CreateElicitationRequest],
		register: (*ClientConn).registerElicitation,
		serve:    (*ClientConn).serveElicitation,
	},
}

// register decides a call in wire order: nothing before initialize is accepted,
// then the capability gate, the method's handler, and its params, decoded once.
func (c *ClientConn) register(call *inboundRequest, request *jsonrpc.Request) registration {
	agreed := c.negotiation.agreed()
	if agreed == nil {
		return refuseBeforeInitialize(request.Method)
	}
	binding, served := clientMethods[request.Method]
	if !served {
		if isStandardMethod(request.Method) {
			return answered(nil, methodNotImplemented(request.Method))
		}
		call.params = request.Params
		return dispatched(callHooks{})
	}
	if refusal := agreed.refusesInbound(request.Method); refusal != nil {
		return answered(nil, refusal)
	}
	if !binding.served(&c.client.config) {
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

func (c *ClientConn) serve(ctx context.Context, call *inboundRequest) (any, error) {
	if binding, standard := clientMethods[call.method]; standard {
		return binding.serve(c, ctx, call)
	}
	return serveExtensionCall(ctx, c, call, c.client.config.CallFallback)
}

func (c *ClientConn) serveNotification(ctx context.Context, request *jsonrpc.Request) error {
	agreed := c.negotiation.agreed()
	if agreed == nil {
		return fmt.Errorf("acp: notification %q arrived before initialize", request.Method)
	}
	switch request.Method {
	case methodSessionUpdate:
		params, err := decodeParams[SessionNotification](request.Params)
		if err != nil {
			return invalidParams(request.Method, err)
		}
		c.client.config.SessionUpdate(ctx, c, params)
		return nil
	case methodElicitationComplete:
		if refusal := agreed.refusesInbound(request.Method); refusal != nil {
			return refusal
		}
		params, err := decodeParams[CompleteElicitationNotification](request.Params)
		if err != nil {
			return invalidParams(request.Method, err)
		}
		c.serveCompletion(ctx, params)
		return nil
	default:
		if isStandardMethod(request.Method) {
			return methodNotImplemented(request.Method)
		}
		serveExtensionNotification(ctx, c, request.Method, request.Params, c.client.config.NotifyFallback)
		return nil
	}
}

// registerPermission files a permission request under its session's turn in wire
// order. One arriving while the turn is being cancelled is answered cancelled
// without a dialog the user would see and have thrown away.
func (c *ClientConn) registerPermission(call *inboundRequest) registration {
	request := paramsOf[RequestPermissionRequest](call)
	admission, turn := c.turns.registerPermission(request.SessionID, call)
	switch admission {
	case permissionCancelled:
		return answered(cancelledPermission(), nil)
	case permissionOwned:
		return dispatched(callHooks{finished: func(any) { turn.forgetPermission(call) }})
	default:
		return dispatched(callHooks{})
	}
}

// servePermission holds the handler's outcome to the options the agent offered.
func (c *ClientConn) servePermission(ctx context.Context, call *inboundRequest) (any, error) {
	request := paramsOf[RequestPermissionRequest](call)
	offered, err := ownPermissionOptions(request.Options)
	if err != nil {
		return nil, newError(ErrorCodeInvalidParams, "%s", err)
	}
	response, err := c.client.config.RequestPermission(ctx, c, request)
	if err != nil || response == nil {
		return handlerAnswer(call.method, response, err)
	}
	if invalid := offered.validate(response.Outcome); invalid != nil {
		return nil, fmt.Errorf("acp: the permission handler's outcome is invalid: %w", invalid)
	}
	return response, nil
}

// answerCancelled claims a pending permission request now, on the cancelling
// goroutine, because the claim decides the race with the handler.
func (c *ClientConn) answerCancelled(call *inboundRequest) func() any {
	return c.claimResponse(call, cancelledPermission(), nil)
}

func cancelledPermission() *RequestPermissionResponse {
	return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}
}
