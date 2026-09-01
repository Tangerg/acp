package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sync"

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

	mu    sync.Mutex
	conns []*AgentConn
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
	if err := checkAuthMethods(config); err != nil {
		return nil, err
	}

	capabilities, err := config.resolveCapabilities()
	if err != nil {
		return nil, err
	}
	return &Agent{config: config.clone(), capabilities: capabilities}, nil
}

// checkAuthMethods holds the handler requirement to the methods that need one.
//
// A terminal method is not served by this agent at all: the client runs the agent
// again as an interactive process, and the schema says the client "MUST NOT pass
// this method to authenticate". Requiring an Authenticate handler for it rejected
// a valid terminal-only agent, while the arm that does need the handler is the
// agent one.
func checkAuthMethods(config *AgentConfig) error {
	if config.Authenticate != nil {
		return nil
	}
	for _, method := range config.AuthMethods {
		if _, terminal := method.(*AuthMethodTerminal); terminal {
			continue
		}
		return errors.New("acp: this agent offers an authentication method it handles itself and has " +
			"no Authenticate handler, so a client that took it up would be refused the method it was " +
			"told to call")
	}
	return nil
}

func (config *AgentConfig) resolveCapabilities() (AgentCapabilities, error) {
	derived := AgentCapabilities{LoadSession: config.LoadSession != nil}
	if config.Capabilities == nil {
		return derived, nil
	}

	stated := deepCopy(*config.Capabilities)
	exceeded := checkAdvertisement(
		PeerInfo{AgentCapabilities: stated}, sideAgent, config.implements)
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
	copied.AuthMethods = deepCopy(config.AuthMethods)
	copied.Capabilities = deepCopy(config.Capabilities)
	return copied
}

// Connect accepts a connection and returns once the read loop is running, before
// any client has sent initialize.
//
// It cannot wait for a handshake it does not control — this side answers one, it
// does not perform one — which is the asymmetry with [Client.Connect]. Until
// initialize arrives the connection answers every other method with -32600, and a
// second initialize on the same connection is -32600 as well.
//
// The context scopes setup only. Lifetime is owned by [AgentConn.Close] and
// observed by [AgentConn.Wait]; [Agent.Run] is the exception and says so.
func (a *Agent) Connect(ctx context.Context, transport Transport) (*AgentConn, error) {
	stream, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	connection := &AgentConn{agent: a, sessions: make(map[SessionID]*AgentSession)}
	connection.conn = newConn(stream, connection.serve, connection.registerInbound, a.config.Logger)
	connection.conn.run()

	a.mu.Lock()
	a.conns = append(a.conns, connection)
	a.mu.Unlock()
	return connection, nil
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
func (a *Agent) Conns() iter.Seq[*AgentConn] {
	a.mu.Lock()
	open := make([]*AgentConn, 0, len(a.conns))
	for _, connection := range a.conns {
		if !connection.conn.ended() {
			open = append(open, connection)
		}
	}
	a.conns = open
	a.mu.Unlock()
	return slices.Values(open)
}

// An AgentConn is one logical connection to a client.
//
// It is not a mirror of [ClientConn]: it serves rather than drives, so it has no
// session-creating methods. Sessions reach an agent through its handlers.
type AgentConn struct {
	agent *Agent
	conn  *conn

	peerMu sync.Mutex
	peer   PeerInfo
	// initialized opens inbound dispatch, and is set when the handshake is
	// negotiated. It cannot wait for the answer to be written: the client may send
	// its next request the moment it reads that answer, and this side would refuse
	// the request it had just made possible.
	initialized bool
	// opened opens this side's own outbound operations, and is set once the answer
	// is on the wire. That is a different moment and a different question: an agent
	// that sent on the strength of its own decision could put a message ahead of
	// the response that told the client what the decision was.
	opened bool

	sessionsMu sync.Mutex
	sessions   map[SessionID]*AgentSession

	// turns is this agent's per-session turn state: which request carries the
	// turn, and whether it was cancelled before its handler started. One per
	// session, because session/cancel names no turn. See cancel.go.
	turnsMu sync.Mutex
	turns   map[SessionID]*agentTurn
}

// Peer reports what initialize negotiated, as a copy. Before initialize it is the
// zero value.
func (c *AgentConn) Peer() PeerInfo {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	return c.peer.clone()
}

// Call sends an extension request. Extension methods only; see [ClientConn.Call].
//
// It fails until the handshake is complete and the client has been told so.
// Connect returns before the handshake arrives — this side answers one, it does
// not perform one — so an agent that sent on the connection immediately would be
// sending before anybody had agreed what the connection can carry, and one that
// sent the instant it decided could put a message ahead of the response carrying
// the decision.
//
// Everything else this side sends is reached from a handler, and a handler runs
// for a request the client sent after it was answered.
func (c *AgentConn) Call(ctx context.Context, method string, params, result any) error {
	if err := c.requireInitialized(method); err != nil {
		return err
	}
	return extensionCall(ctx, c.conn, method, params, result)
}

// Notify sends an extension notification. Extension methods only, and not before
// initialize; see [AgentConn.Call].
func (c *AgentConn) Notify(ctx context.Context, method string, params any) error {
	if err := c.requireInitialized(method); err != nil {
		return err
	}
	return extensionNotify(ctx, c.conn, method, params)
}

// ErrNotInitialized is returned by an operation attempted on an agent connection
// before the client has completed the handshake.
//
// An agent cannot make it happen sooner: the client initiates initialize. What it
// can do is wait for a session to reach one of its handlers, which cannot happen
// before initialization.
var ErrNotInitialized = errors.New("acp: this connection has not been initialized yet")

func (c *AgentConn) requireInitialized(method string) error {
	if c.isOpen() {
		return nil
	}
	return fmt.Errorf("acp: %s cannot be sent yet: %w", method, ErrNotInitialized)
}

// Close ends the connection. It is idempotent and safe to call concurrently.
func (c *AgentConn) Close() error {
	return c.conn.close()
}

// Wait blocks until the connection has ended and reports its terminal error.
func (c *AgentConn) Wait() error {
	return c.conn.wait()
}

// initialize prepares the answer to the handshake.
//
// It does not open the connection. What it negotiated is held until the answer
// has been written, because an agent that started sending on the strength of its
// own decision could put a message ahead of the response that told the client
// what the decision was. publishPeer is the other half, and the request lifecycle
// runs it once the answer is on the wire.
//
// The answer is built per connection and not copied from the configuration. Two
// of its fields depend on what this client offered, and the schema says so: a
// terminal authentication method may be advertised only to a client that enabled
// it, and the position encoding is "selected by the agent from the client's
// supported encodings".
func (c *AgentConn) initialize(request *InitializeRequest) (*InitializeResponse, error) {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()

	if c.initialized {
		// A second initialize is not a re-negotiation. The capabilities already
		// gate work in flight, and letting them change under it would make the
		// authority boundary depend on timing.
		return nil, newError(ErrorCodeInvalidRequest, "this connection is already initialized")
	}

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
	capabilities.PositionEncoding = selectPositionEncoding(capabilities, request.ClientCapabilities)

	response := &InitializeResponse{
		ProtocolVersion:   version,
		AgentCapabilities: capabilities,
		AuthMethods:       c.agent.offeredAuthMethods(request.ClientCapabilities),
	}
	if info := c.agent.config.Info; info != nil {
		response.AgentInfo = OptValue(deepCopy(*info))
	}
	if len(c.agent.config.Meta) > 0 {
		response.Meta = OptValue(deepCopy(c.agent.config.Meta))
	}

	c.peer = PeerInfo{
		ProtocolVersion:    version,
		ClientCapabilities: deepCopy(request.ClientCapabilities),
		ClientInfo:         request.ClientInfo,
		ClientMeta:         request.Meta,
		AgentCapabilities:  response.AgentCapabilities,
		AgentInfo:          response.AgentInfo,
		AgentMeta:          response.Meta,
		AuthMethods:        response.AuthMethods,
	}
	c.initialized = true
	return response, nil
}

// openOutbound lets this side send, once the client has been told what it agreed
// to. It is a no-op when the handshake failed: there is nothing to open.
func (c *AgentConn) openOutbound() {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	c.opened = c.initialized
}

// offeredAuthMethods is what this agent may advertise to this client.
//
// The schema is explicit about the terminal arm: an agent "MUST advertise this
// method only when the client enabled its terminal authentication capability".
// Whether it may be offered is therefore a fact about the connection, and sending
// the configured list unfiltered produced a schema-invalid handshake against any
// client that had not opted in.
func (a *Agent) offeredAuthMethods(client ClientCapabilities) []AuthMethod {
	offered := make([]AuthMethod, 0, len(a.config.AuthMethods))
	for _, method := range a.config.AuthMethods {
		if _, terminal := method.(*AuthMethodTerminal); terminal && !client.Auth.Terminal {
			continue
		}
		offered = append(offered, deepCopy(method))
	}
	if len(offered) == 0 {
		return nil
	}
	return offered
}

// selectPositionEncoding narrows the configured encoding to one the client
// offered.
//
// The schema calls the field "the position encoding selected by the agent from
// the client's supported encodings", so an encoding the client never offered is
// not a selection. Omitting it leaves both peers on the default rather than on
// two different answers about what a character offset counts.
func selectPositionEncoding(agent AgentCapabilities, client ClientCapabilities) Opt[PositionEncodingKind] {
	chosen, configured := agent.PositionEncoding.Get()
	if configured && slices.Contains(client.PositionEncodings, chosen) {
		return OptValue(chosen)
	}
	return Opt[PositionEncodingKind]{}
}

func (c *AgentConn) isInitialized() bool {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	return c.initialized
}

func (c *AgentConn) isOpen() bool {
	c.peerMu.Lock()
	defer c.peerMu.Unlock()
	return c.opened
}

// serve dispatches the requests a client makes of an agent.
func (c *AgentConn) serve(ctx context.Context, request *jsonrpc.Request) (any, error) {
	config := &c.agent.config

	if request.Method == methodInitialize {
		params, err := decodeParams[InitializeRequest](request)
		if err != nil {
			return nil, err
		}
		return c.initialize(params)
	}

	// The only legal inbound message before initialize is initialize itself.
	// Serving anything else would mean serving it under capabilities nobody has
	// exchanged.
	if !c.isInitialized() {
		return nil, newError(ErrorCodeInvalidRequest,
			"%s arrived before initialize", request.Method)
	}

	if isStandardMethod(request.Method) {
		if allowed, capability := allowsMethod(c.Peer(), request.Method); !allowed {
			return nil, methodRefusal(request.Method, capability)
		}
	}

	switch request.Method {
	case methodAuthenticate:
		return dispatchCall(ctx, request, config.Authenticate)
	case methodSessionNew:
		return c.newSession(ctx, request)
	case methodSessionLoad:
		return dispatchSessionCall(ctx, c, request, config.LoadSession)
	case methodSessionSetMode:
		return dispatchSessionCall(ctx, c, request, config.SetMode)
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
