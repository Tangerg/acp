package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"

	"github.com/Tangerg/acp/jsonrpc"
)

// An AgentConfig is everything an agent is built from.
//
// It is the mirror of [ClientConfig] in structure and not in content: an agent
// serves rather than drives, so its fields are the operations a client calls on
// it.
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
	NewSession func(ctx context.Context, request *NewSessionRequest) (*NewSessionResponse, error)
	Prompt     func(ctx context.Context, session *AgentSession, request *PromptRequest) (*PromptResponse, error)
	Cancel     func(ctx context.Context, session *AgentSession, notification *CancelNotification)

	// Authenticate answers the method a client calls after being told to. It is
	// not gated on a capability — an agent asks for authentication by listing
	// AuthMethods, or by answering session/new with -32000 — so a nil handler here
	// simply means this agent requires none.
	Authenticate func(ctx context.Context, request *AuthenticateRequest) (*AuthenticateResponse, error)

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
	Logout func(ctx context.Context, request *LogoutRequest) (*LogoutResponse, error)

	// The session lifecycle, each gated on its own property of
	// agentCapabilities.sessionCapabilities and each advertised by being set.
	//
	// ListSession and DeleteSession take no handle: the schema describes deletion
	// as removing a session "from session/list", so both name sessions this
	// connection may never have opened, and minting a handle for one would spend
	// the connection's session budget on a session nobody is in.
	ListSessions  func(ctx context.Context, request *ListSessionsRequest) (*ListSessionsResponse, error)
	DeleteSession func(ctx context.Context, request *DeleteSessionRequest) (*DeleteSessionResponse, error)

	// ResumeSession reopens a session without replaying it, which is what the
	// schema says distinguishes it from session/load.
	ResumeSession func(
		ctx context.Context,
		session *AgentSession,
		request *ResumeSessionRequest,
	) (*ResumeSessionResponse, error)

	// CloseSession frees what a session holds. The connection has already
	// cancelled the session's turn by the time this runs; see [AgentConn.serve].
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
}

// An Agent is the program that uses a model to read and change a workspace.
//
// One agent may serve many connections. Ordinarily there is one — a client starts
// the agent as a subprocess and speaks over its stdin and stdout — but nothing
// here assumes that.
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
		return nil, fmt.Errorf("acp: an agent needs these baseline handlers, which are the whole of a "+
			"turn and cannot be optional: %v", missing)
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

// authenticate serves authenticate, holding the identifier to what this
// connection advertised.
//
// The same rule as on the client side, kept here against any peer: a client that
// does not go through this package can still name a method that was never
// offered, and the handler would then be asked to authenticate something the
// agent never said it could.
func (c *AgentConn) authenticate(ctx context.Context, request *jsonrpc.Request) (any, error) {
	params, err := decodeParams[AuthenticateRequest](request)
	if err != nil {
		return nil, err
	}
	if refusal := c.Peer().authenticates(params.MethodID); refusal != nil {
		return nil, refusal
	}
	if c.agent.config.Authenticate == nil {
		return nil, newError(ErrorCodeMethodNotFound, "%s is not implemented here", request.Method)
	}
	response, err := c.agent.config.Authenticate(ctx, params)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, newError(ErrorCodeInternalError, "the handler for %s returned nothing", request.Method)
	}
	return response, nil
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
			"acp: the stated capabilities advertise what this agent cannot serve: %v", exceeded)
	}
	return stated, nil
}

// implements reports whether this configuration has the handler that serves an
// agent method. See [ClientConfig.implements].
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

// clone copies every mutable value the library keeps reading after construction.
// See [ClientConfig.clone].
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
	conn.link = newLink(stream, conn, a.config.Logger)
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

// Conns iterates the connections this agent is serving.
func (a *Agent) Conns() iter.Seq[*AgentConn] { return a.conns.all() }

// An AgentConn is one logical connection to a client.
//
// It is not a mirror of [ClientConn]: it serves rather than drives, so it has no
// session-creating methods. Sessions reach an agent through its handlers.
type AgentConn struct {
	connection

	agent    *Agent
	sessions sessions[AgentSession]

	turns agentTurns
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

func (c *AgentConn) awaitHandshake(ctx context.Context, method string) error {
	select {
	case <-c.handshake.whenPublished():
		return nil
	case <-c.over():
		return c.failure()
	case <-ctx.Done():
		return fmt.Errorf("acp: %s waited for the client to initialize: %w", method, ctx.Err())
	}
}

// initialize prepares the answer to the handshake.
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
	// This package speaks version 1 and nothing else, so the answer is always the
	// version it implements — not the lower of the two.
	//
	// Taking the minimum was wrong in a way that matters: a client asking for
	// version 0 would have been told 0, and this agent would then have claimed a
	// grammar it does not have. The schema's rule is that an agent answers with
	// the requested version when it supports it and with its own latest otherwise,
	// and that the client disconnects if it cannot speak the answer. For a
	// single-version package both branches are the same number.
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

// serve dispatches the requests a client makes of an agent.
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
			"%s arrived before initialize", request.Method)
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
		return dispatchCall(ctx, request, config.Logout)
	case methodSessionList:
		return dispatchCall(ctx, request, config.ListSessions)
	case methodSessionDelete:
		return dispatchCall(ctx, request, config.DeleteSession)
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
		return nil, newError(ErrorCodeMethodNotFound,
			"%s is not a method this agent serves", request.Method)
	}
	return dispatchExtension(ctx, request, config.CallFallback, config.NotifyFallback)
}
