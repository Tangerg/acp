package acp

import (
	"context"
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
	info         *Implementation
	meta         Meta
	logger       *slog.Logger
	limits       Limits
	capabilities ClientCapabilities
	dispatcher   *dispatcher[*ClientConn]
	conns        registry[*ClientConn]
}

// NewClient builds a client, or reports why the configuration cannot be served.
// The handlers are bound here, so changing the configuration afterwards changes
// nothing a running client advertises or serves.
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
	dispatcher := newDispatcher(sideClient, config.methods(), config.CallFallback, config.NotifyFallback)
	capabilities, err := config.resolveCapabilities(dispatcher.serves)
	if err != nil {
		return nil, err
	}
	return &Client{
		info:         deepCopy(config.Info),
		meta:         deepCopy(config.Meta),
		logger:       config.Logger,
		limits:       config.Limits,
		capabilities: capabilities,
		dispatcher:   dispatcher,
	}, nil
}

// methods binds every standard method a client serves to this configuration's
// handlers. A handler that is not set leaves its method unbound.
func (c *ClientConfig) methods() map[string]*methodBinding[*ClientConn] {
	methods := map[string]*methodBinding[*ClientConn]{
		methodSessionUpdate: notified(func(conn *ClientConn, ctx context.Context, notification *SessionNotification) {
			c.SessionUpdate(ctx, conn, notification)
		}),
		methodSessionRequestPermission: answering(servePermission(c.RequestPermission)).
			registeredBy((*ClientConn).registerPermission),
		methodFsReadTextFile:  handled(c.ReadTextFile),
		methodFsWriteTextFile: handled(c.WriteTextFile),
	}
	if terminal := c.Terminal; terminal != nil {
		methods[methodTerminalCreate] = handled(terminal.Create)
		methods[methodTerminalOutput] = handled(terminal.Output)
		methods[methodTerminalWaitForExit] = handled(terminal.WaitForExit)
		methods[methodTerminalKill] = handled(terminal.Kill)
		methods[methodTerminalRelease] = handled(terminal.Release)
	}
	if elicitation := c.Elicitation; elicitation != nil {
		handlers := *elicitation
		methods[methodElicitationCreate] = answering(serveElicitation(handlers)).
			registeredBy(registerElicitation(handlers))
		// The connection tracks accepted URL interactions whether or not the
		// application wants to hear about their completion.
		if handlers.URL != nil {
			methods[methodElicitationComplete] = notified(serveCompletion(handlers.Complete))
		}
	}
	return methods
}

func (c *ClientConfig) resolveCapabilities(serves func(method string) bool) (ClientCapabilities, error) {
	if c.Capabilities == nil {
		derived := advertise[ClientCapabilities](serves)
		derived.Elicitation = c.Elicitation.capabilities()
		return derived, nil
	}
	stated := deepCopy(*c.Capabilities)
	unserved := append(unservedMethods(stated, serves), c.Elicitation.unserved(stated)...)
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
	served := &dispatch[*ClientConn]{dispatcher: client.dispatcher, conn: conn, negotiation: conn.negotiation}
	conn.link = newLink(stream, served, client.logger, client.limits)
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
// [ClientConn.Negotiated], which is how a client answers [ErrAuthRequired]. An
// identifier the agent did not offer, or one naming a terminal method, is
// refused with [ErrNotAdvertised] before anything is sent.
func (c *ClientConn) Authenticate(ctx context.Context, params *AuthenticateRequest) (*AuthenticateResponse, error) {
	if params == nil {
		params = new(AuthenticateRequest)
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
	if errors.Is(err, ErrResourceNotFound) {
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
	client := c.client
	request := &InitializeRequest{
		ProtocolVersion:    CurrentProtocolVersion,
		ClientCapabilities: client.capabilities,
	}
	if client.info != nil {
		request.ClientInfo = OptValue(deepCopy(*client.info))
	}
	if client.meta.Len() > 0 {
		request.Meta = OptValue(deepCopy(client.meta))
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

// servePermission holds the handler's outcome to the options the agent offered,
// as they were offered: the handler may edit the request it is given.
func servePermission(
	handler func(context.Context, *ClientConn, *RequestPermissionRequest) (*RequestPermissionResponse, error),
) func(*ClientConn, context.Context, *inboundRequest, *RequestPermissionRequest) (*RequestPermissionResponse, error) {
	return func(c *ClientConn, ctx context.Context, _ *inboundRequest, request *RequestPermissionRequest) (
		*RequestPermissionResponse, error,
	) {
		if err := distinctOptions(request.Options); err != nil {
			return nil, newError(ErrorCodeInvalidParams, "%s", err)
		}
		offered := offeredOptions(request.Options)
		response, err := handler(ctx, c, request)
		if err != nil || response == nil {
			return response, err
		}
		if invalid := offered.check(response.Outcome); invalid != nil {
			return nil, fmt.Errorf("acp: the permission handler's outcome is invalid: %w", invalid)
		}
		return response, nil
	}
}

// answerCancelled claims a pending permission request now, on the cancelling
// goroutine, because the claim decides the race with the handler.
func (c *ClientConn) answerCancelled(call *inboundRequest) func() any {
	return c.claimResponse(call, cancelledPermission(), nil)
}

func cancelledPermission() *RequestPermissionResponse {
	return &RequestPermissionResponse{Outcome: &RequestPermissionOutcomeCancelled{}}
}
