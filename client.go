package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"

	"github.com/Tangerg/acp/jsonrpc"
)

// ClientConfig defines a client's identity, handlers, and capability
// advertisement. SessionUpdate and RequestPermission are required. Optional
// handlers derive their corresponding capabilities unless Capabilities is set.
type ClientConfig struct {
	// Info identifies this client during initialize. Optional: the schema lets a
	// peer stay anonymous.
	Info *Implementation
	// Meta is attached to this client's initialize request.
	Meta Meta
	// Logger receives handler details withheld from the peer and notification
	// failures that have no response channel. nil discards them. The package does
	// not write diagnostics to process-global output.
	Logger *slog.Logger

	// SessionUpdate receives the agent's running commentary for a turn. Baseline:
	// a client that cannot accept these cannot watch a turn happen.
	SessionUpdate func(ctx context.Context, notification *SessionNotification)

	// RequestPermission asks the user to approve a tool call. Baseline, and
	// required: there is no safe nil handler for it.
	//
	// The wire outcome is either cancelled or a selected option identifier, and an
	// agent is not obliged to offer a reject option — so there is no universal
	// "deny" to synthesise for a client that will not answer. A missing handler
	// therefore fails construction rather than inventing an outcome the protocol
	// does not define.
	RequestPermission func(ctx context.Context, request *RequestPermissionRequest) (*RequestPermissionResponse, error)

	// ReadTextFile and WriteTextFile are gated on two independent capability
	// booleans, so they are two independent handlers.
	ReadTextFile  func(ctx context.Context, request *ReadTextFileRequest) (*ReadTextFileResponse, error)
	WriteTextFile func(ctx context.Context, request *WriteTextFileRequest) (*WriteTextFileResponse, error)

	// Terminal is all five methods or none, because the capability that gates them
	// is one boolean covering all five.
	Terminal *TerminalHandlers

	// Elicitation is this client's ability to put a structured request in front of
	// the user. Its two modes are two independent capabilities, so it is a group
	// whose fields advertise themselves rather than an all-or-none set.
	Elicitation *ElicitationHandlers

	// CallFallback and NotifyFallback receive extension methods: names outside the
	// set the specification defines. A standard method never reaches them, however
	// it is spelled.
	CallFallback   func(ctx context.Context, request *ExtRequest) (json.RawMessage, error)
	NotifyFallback func(ctx context.Context, notification *ExtNotification)

	// Capabilities is the complete desired advertisement, never a patch.
	//
	// nil advertises exactly what the handlers above support. Non-nil is taken as
	// stated, and construction fails if it claims anything the handlers do not
	// implement — capabilities are an authority boundary, and advertising one this
	// client cannot serve is a promise it will break.
	//
	// It is a replacement rather than a merge because a field-by-field merge would
	// need to distinguish "set to false" from "not set", and a scalar boolean has
	// no third state to express that.
	Capabilities *ClientCapabilities

	// Limits bounds what each of this client's connections will hold on an agent's
	// behalf. The zero value takes every default; see [Limits] for which bound a
	// slow SessionUpdate handler is the one to raise.
	Limits Limits
}

// TerminalHandlers is the complete terminal capability. When non-nil, all five
// handlers are required because the protocol advertises them with one boolean.
type TerminalHandlers struct {
	Create      func(ctx context.Context, request *CreateTerminalRequest) (*CreateTerminalResponse, error)
	Output      func(ctx context.Context, request *TerminalOutputRequest) (*TerminalOutputResponse, error)
	WaitForExit func(ctx context.Context, request *WaitForTerminalExitRequest) (*WaitForTerminalExitResponse, error)
	Kill        func(ctx context.Context, request *KillTerminalRequest) (*KillTerminalResponse, error)
	Release     func(ctx context.Context, request *ReleaseTerminalRequest) (*ReleaseTerminalResponse, error)
}

// A Client owns the workspace-facing handlers and capabilities shared by its
// connections. One client may connect to many agents.
type Client struct {
	config       ClientConfig
	capabilities ClientCapabilities
	conns        registry[*ClientConn]
}

// NewClient builds a client, or reports why the configuration cannot be served.
//
// It returns an error because the capability invariant is checked here: a
// configuration whose advertisement exceeds its implementation, or that omits a
// baseline handler, is rejected before it can accept a request it cannot serve.
func NewClient(config *ClientConfig) (*Client, error) {
	if config == nil {
		config = &ClientConfig{}
	}
	if config.SessionUpdate == nil {
		return nil, errors.New("acp: ClientConfig.SessionUpdate is required")
	}
	if config.RequestPermission == nil {
		return nil, errors.New("acp: ClientConfig.RequestPermission is required")
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
	return &Client{config: config.clone(), capabilities: capabilities}, nil
}

// check refuses a partial handler set, because the capability that gates them is
// one boolean covering all five methods: a client with four of them would refuse a
// method it had advertised.
func (handlers *TerminalHandlers) check() error {
	if handlers == nil {
		return nil
	}
	missing := make([]string, 0, 5)
	for name, present := range map[string]bool{
		"Create":      handlers.Create != nil,
		"Kill":        handlers.Kill != nil,
		"Output":      handlers.Output != nil,
		"Release":     handlers.Release != nil,
		"WaitForExit": handlers.WaitForExit != nil,
	} {
		if !present {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("acp: TerminalHandlers is incomplete; missing: %v", missing)
}

func (config *ClientConfig) resolveCapabilities() (ClientCapabilities, error) {
	derived := ClientCapabilities{
		Fs: FileSystemCapabilities{
			ReadTextFile:  config.ReadTextFile != nil,
			WriteTextFile: config.WriteTextFile != nil,
		},
		Terminal:    config.Terminal != nil,
		Elicitation: config.Elicitation.capabilities(),
	}
	if config.Capabilities == nil {
		return derived, nil
	}

	stated := deepCopy(*config.Capabilities)
	exceeded := gates.exceeded(PeerInfo{ClientCapabilities: stated}, sideClient, config.implements)
	// The elicitation modes are checked beside the methods rather than after them,
	// because an advertisement is one promise: a mode with no handler is refused on
	// its first use exactly as a method with no handler is.
	exceeded = append(exceeded, exceededElicitationModes(stated, config.Elicitation)...)
	if len(exceeded) > 0 {
		return ClientCapabilities{}, fmt.Errorf(
			"acp: ClientConfig.Capabilities advertises what this client cannot serve: %v", exceeded)
	}
	return stated, nil
}

// The other half of the capability table: the table says which capability gates
// which method, this says which methods a configuration can actually serve, and
// neither is derivable from the other. Construction checks their intersection.
func (config *ClientConfig) implements(method string) bool {
	switch method {
	case methodSessionUpdate, methodSessionRequestPermission:
		return true // baseline, and NewClient has already refused a nil handler
	case methodFsReadTextFile:
		return config.ReadTextFile != nil
	case methodFsWriteTextFile:
		return config.WriteTextFile != nil
	case methodTerminalCreate, methodTerminalOutput, methodTerminalWaitForExit,
		methodTerminalKill, methodTerminalRelease:
		return config.Terminal != nil
	case methodElicitationCreate:
		// Either mode serves the method; which modes it can render is the parameter
		// capability, and the handler group has already refused serving neither.
		return config.Elicitation != nil
	case methodElicitationComplete:
		// Serving the url mode is what makes a client able to accept completions.
		// Whether one reaches the application is Complete's business: the protocol
		// makes sending optional and requires a client to ignore a completion it
		// does not recognise, so a client with no Complete handler is not one that
		// cannot serve the method.
		return config.Elicitation != nil && config.Elicitation.URL != nil
	default:
		return false
	}
}

// Without it a caller's slice, map or handler set stays aliased: mutating it
// afterwards would change what this client advertises, or what it serves, without
// going back through the check that made it valid — and would race a connection
// already reading it.
func (config *ClientConfig) clone() ClientConfig {
	copied := *config
	copied.Info = deepCopy(config.Info)
	copied.Meta = deepCopy(config.Meta)
	copied.Capabilities = deepCopy(config.Capabilities)
	// Every handler group is copied, not aliased. A caller who kept a pointer
	// could otherwise swap a handler after NewClient validated the set, changing
	// what a running client serves without going back through the check that
	// made it valid — and racing the dispatcher that reads it.
	if config.Terminal != nil {
		handlers := *config.Terminal
		copied.Terminal = &handlers
	}
	if config.Elicitation != nil {
		handlers := *config.Elicitation
		copied.Elicitation = &handlers
	}
	return copied
}

// Connect performs the handshake and returns a connection that is already
// initialized.
//
// It returns only after the transport connects, initialize succeeds, the
// negotiated version is one this package implements, and the peer's capabilities
// are stored. If any of those fails — including the context being cancelled
// mid-handshake — the logical connection is closed before returning, so a failed
// Connect never leaks a live transport or a half-initialized peer.
//
// There is no public Initialize for the same reason: it would make three failure
// modes this API has to define and test — a call before initialization, a second
// one, and two concurrent ones — and doing it here makes all three
// unrepresentable.
//
// The context scopes setup only. A caller who passed a five-second handshake
// timeout has not asked for the connection to die after five seconds; lifetime is
// owned by [ClientConn.Close] and observed by [ClientConn.Wait].
func (c *Client) Connect(ctx context.Context, transport Transport) (*ClientConn, error) {
	stream, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	conn := &ClientConn{connection: newConnection(), client: c}
	conn.link = newLink(stream, conn, c.config.Logger, c.config.Limits)
	conn.elicitations.limit = conn.limits.OutstandingElicitations
	conn.run()

	if err := conn.initialize(ctx); err != nil {
		// Close before returning: a half-initialized connection is not something to
		// hand back and hope nobody uses.
		_ = conn.Close()
		return nil, err
	}

	c.conns.add(conn)
	return conn, nil
}

func (c *Client) Conns() iter.Seq[*ClientConn] { return c.conns.all() }

// A ClientConn is one logical connection to an agent, after initialize.
//
// It is named for the connection and not the session because the specification
// already means something by "session": session/new returns a session
// identifier, a session is a conversation with its own history, and one
// connection carries many. Calling the connection a session too would have put
// NewSession on a type called ClientSession.
type ClientConn struct {
	connection

	client   *Client
	sessions sessions[ClientSession]

	turns        clientTurns
	elicitations urlElicitations
}

func (c *ClientConn) initialize(ctx context.Context) error {
	if !c.handshake.begin() {
		return errors.New("acp: initialize is already in progress on this connection")
	}
	request := &InitializeRequest{
		ProtocolVersion:    CurrentProtocolVersion,
		ClientCapabilities: c.client.capabilities,
	}
	if info := c.client.config.Info; info != nil {
		request.ClientInfo = OptValue(*info)
	}
	if c.client.config.Meta.Len() > 0 {
		request.Meta = OptValue(deepCopy(c.client.config.Meta))
	}

	// The answer is checked and published from the delivery loop, so that anything
	// the agent sent after answering is served by a connection that already knows
	// what was agreed. See link.handshake.
	return c.callHandshake(ctx, methodInitialize, request, func(answer *jsonrpc.Response) error {
		var response InitializeResponse
		if err := decodeResponse(answer, &response); err != nil {
			return err
		}
		return c.accept(request, &response)
	})
}

func (c *ClientConn) accept(request *InitializeRequest, response *InitializeResponse) error {
	// A protocol version identifies a grammar, not a feature level. Accepting a
	// higher or lower number would claim a grammar this package does not implement.
	if response.ProtocolVersion != CurrentProtocolVersion {
		return fmt.Errorf("acp: the agent answered initialize with protocol version %d, "+
			"and this package implements %d and no other",
			response.ProtocolVersion, CurrentProtocolVersion)
	}
	auth, err := ownAuthenticationMethods(response.AuthMethods)
	if err != nil {
		return fmt.Errorf("acp: initialize response contains invalid authentication methods: %w", err)
	}
	if err := auth.validateOffer(request.ClientCapabilities); err != nil {
		return err
	}
	response.AuthMethods = auth

	c.handshake.accept(PeerInfo{
		ProtocolVersion:    response.ProtocolVersion,
		ClientCapabilities: deepCopy(c.client.capabilities),
		ClientInfo:         request.ClientInfo,
		ClientMeta:         request.Meta,
		AgentCapabilities:  response.AgentCapabilities,
		AgentInfo:          response.AgentInfo,
		AgentMeta:          response.Meta,
		AuthMethods:        response.AuthMethods,
	})
	c.handshake.publish()
	return nil
}

// Call sends an extension request and decodes its result.
//
// Extension methods only. A method the specification defines is refused, because
// it has exactly one path through the typed codec and the capability gate, and
// this is not it.
func (c *ClientConn) Call(ctx context.Context, method string, params, result any) error {
	return extensionCall(ctx, c.link, method, params, result)
}

// Notify sends an extension notification. Extension methods only; see
// [ClientConn.Call].
func (c *ClientConn) Notify(ctx context.Context, method string, params any) error {
	return extensionNotify(ctx, c.link, method, params)
}

// awaitHandshake holds an inbound message until this side knows what was
// negotiated, or refuses one that genuinely arrived first.
//
// The delivery queue processes the initialize answer before every message that
// followed it, so an unaccepted handshake here proves the peer sent the request
// too early rather than exposing a publication race.
func (c *ClientConn) awaitHandshake(request *jsonrpc.Request) error {
	if c.handshake.isAccepted() {
		return nil
	}
	return newError(ErrorCodeInvalidRequest,
		"method %q arrived before initialize completed", request.Method)
}

func (c *ClientConn) serve(ctx context.Context, request *jsonrpc.Request) (any, error) {
	config := &c.client.config

	if err := c.awaitHandshake(request); err != nil {
		return nil, err
	}

	// The capability gate first, and in this direction too. Capabilities are an
	// authority boundary, so a method this client never advertised is refused
	// rather than served — the agent was told it was not there.
	if isStandardMethod(request.Method) {
		if err := c.Peer().permits(request.Method); err != nil {
			return nil, err
		}
	}

	switch request.Method {
	case methodSessionUpdate:
		return nil, dispatchNotificationContext(ctx, request, config.SessionUpdate)
	case methodSessionRequestPermission:
		return c.requestPermission(ctx, request)
	case methodFsReadTextFile:
		return dispatchCall(ctx, request, config.ReadTextFile)
	case methodFsWriteTextFile:
		return dispatchCall(ctx, request, config.WriteTextFile)
	case methodTerminalCreate:
		return dispatchCall(ctx, request, config.Terminal.Create)
	case methodTerminalOutput:
		return dispatchCall(ctx, request, config.Terminal.Output)
	case methodTerminalWaitForExit:
		return dispatchCall(ctx, request, config.Terminal.WaitForExit)
	case methodTerminalKill:
		return dispatchCall(ctx, request, config.Terminal.Kill)
	case methodTerminalRelease:
		return dispatchCall(ctx, request, config.Terminal.Release)
	case methodElicitationCreate:
		return c.createElicitation(ctx, request)
	case methodElicitationComplete:
		return nil, c.completeElicitation(ctx, request)
	}

	if isStandardMethod(request.Method) {
		return nil, methodNotImplemented(request.Method)
	}
	return dispatchExtension(ctx, request, config.CallFallback, config.NotifyFallback)
}
