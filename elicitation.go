package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Tangerg/acp/jsonrpc"
)

// ElicitationHandlers is a client's ability to put an elicitation in front of the
// user.
//
// The two modes are two independent capabilities the way the two file-system
// booleans are, so they are two fields and not one, and each advertises itself.
// Both arrive through one method, so a mode with no handler is refused with
// invalid-params rather than method-not-found: the method was advertised, and it
// is the mode inside it that was not.
//
// The mode is passed to a handler alongside the request it came from. It is that
// request's own Value, already asserted by the dispatcher that used it to choose
// the handler.
type ElicitationHandlers struct {
	Form func(
		ctx context.Context,
		request *CreateElicitationRequest,
		mode *ElicitationFormMode,
	) (*CreateElicitationResponse, error)

	URL func(
		ctx context.Context,
		request *CreateElicitationRequest,
		mode *ElicitationURLMode,
	) (*CreateElicitationResponse, error)

	// Complete is optional even when URL is set, because the protocol makes
	// sending one optional: an agent "MAY send elicitation/complete". A client
	// that leaves it nil still tracks the elicitation and still ignores a
	// completion it does not recognise; it just does not hear about the ones it
	// does. Setting it without URL is refused, because nothing could ever call it.
	Complete func(ctx context.Context, notification *CompleteElicitationNotification)
}

// check refuses at construction a group that advertises elicitation and renders
// no mode, and one whose completion handler no mode could ever reach.
func (handlers *ElicitationHandlers) check() error {
	if handlers == nil {
		return nil
	}
	if handlers.Form == nil && handlers.URL == nil {
		return errors.New(
			"acp: ElicitationHandlers serves neither mode; set Form, URL, or both")
	}
	if handlers.URL == nil && handlers.Complete != nil {
		return errors.New(
			"acp: ElicitationHandlers.Complete is set without URL, so it could never run")
	}
	return nil
}

// A mode with no handler is left absent rather than set false: the schema's
// capability objects use a present `{}` as the only yes.
func (handlers *ElicitationHandlers) capabilities() Opt[ElicitationCapabilities] {
	if handlers == nil {
		return Opt[ElicitationCapabilities]{}
	}
	var advertised ElicitationCapabilities
	if handlers.Form != nil {
		advertised.Form = OptValue(ElicitationFormCapabilities{})
	}
	if handlers.URL != nil {
		advertised.URL = OptValue(ElicitationURLCapabilities{})
	}
	return OptValue(advertised)
}

// CreateElicitationParams is an elicitation without its scope, because the
// operation supplies that: a session handle elicits within its session and a
// connection within the request it is serving. This is the division every handle
// here makes — a caller never spells a session identifier either — and it is what
// keeps [ElicitationRequestScope] unexported, since the scope it names is a
// JSON-RPC request identifier this API does not hand out.
type CreateElicitationParams struct {
	// Required.
	Message string

	// Required: *[ElicitationFormMode] or *[ElicitationURLMode], with its own
	// Value left nil. A scope set here would be replaced by the operation, and
	// replacing a caller's value in silence is worse than refusing it.
	Mode CreateElicitationRequestValue

	// Session scope only, which is what an agent relaying an MCP server's
	// elicitation during a tool call needs. [AgentConn.CreateElicitation] refuses
	// it rather than dropping it.
	ToolCallID Opt[ToolCallID]

	Meta Opt[Meta]
}

// CreateElicitation asks the user for structured input within this session.
func (s *AgentSession) CreateElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
) (*CreateElicitationResponse, error) {
	scope := func(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
		within := ElicitationSessionScope{SessionID: s.id, ToolCallID: params.ToolCallID}
		switch mode := mode.(type) {
		case *ElicitationFormMode:
			mode.Value = &ElicitationFormModeSession{ElicitationSessionScope: within}
		case *ElicitationURLMode:
			mode.Value = &ElicitationURLModeSession{ElicitationSessionScope: within}
		}
		return mode, nil
	}
	return createElicitation(ctx, s.conn, params, scope)
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
		id, serving := servingRequest(ctx)
		if !serving {
			return nil, errors.New(
				"acp: AgentConn.CreateElicitation is scoped to the request being served, so it " +
					"must be called from a handler with the context that handler was given")
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

// The mode is copied before the scope is written into it: the value is the
// caller's, and an agent holding one prepared schema and eliciting with it twice
// would otherwise find the second call carrying the first call's scope.
func createElicitation(
	ctx context.Context,
	conn *AgentConn,
	params *CreateElicitationParams,
	scope func(CreateElicitationRequestValue) (CreateElicitationRequestValue, error),
) (*CreateElicitationResponse, error) {
	var failed bool
	if params == nil {
		return nil, paramsRequired("CreateElicitation", "Message and Mode")
	}
	mode, modeErr := checkedMode(params.Mode)
	if modeErr != nil {
		return nil, modeErr
	}
	if err := conn.awaitHandshake(ctx, methodElicitationCreate); err != nil {
		return nil, err
	}
	if err := conn.Peer().permits(methodElicitationCreate); err != nil {
		return nil, err
	}
	if err := conn.Peer().permitsElicitationMode(mode); err != nil {
		return nil, err
	}

	scoped, scopeErr := scope(deepCopy(mode))
	if scopeErr != nil {
		return nil, scopeErr
	}

	// A URL elicitation outlives its response, so its identifier is claimed before
	// the write and released if the write never happened. The claim is what keeps
	// it unique among the ones this connection has open.
	if url, isURL := scoped.(*ElicitationURLMode); isURL {
		if err := conn.elicitations.open(url.ElicitationID); err != nil {
			return nil, err
		}
		defer func() {
			if failed {
				conn.elicitations.forget(url.ElicitationID)
			}
		}()
	}

	request := &CreateElicitationRequest{
		Message: params.Message,
		Meta:    params.Meta,
		Value:   scoped,
	}
	response := new(CreateElicitationResponse)
	if err := conn.call(ctx, methodElicitationCreate, request, response); err != nil {
		failed = true
		return nil, err
	}
	return response, nil
}

// The catch-all arm exists so a mode this package does not know survives being
// decoded from a peer. Sending one is the opposite: a discriminant this side made
// up.
func checkedMode(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
	switch mode := mode.(type) {
	case nil:
		return nil, paramsRequired("CreateElicitation", "Mode")

	case *ElicitationFormMode:
		// A typed nil is a legal interface value and reaches this arm, so it is
		// checked before the field is read rather than after.
		if mode == nil {
			return nil, paramsRequired("CreateElicitation", "Mode")
		}
		if mode.Value != nil {
			return nil, errScopeIsTheOperations
		}
		return mode, nil

	case *ElicitationURLMode:
		if mode == nil {
			return nil, paramsRequired("CreateElicitation", "Mode")
		}
		if mode.Value != nil {
			return nil, errScopeIsTheOperations
		}
		return mode, nil

	default:
		return nil, fmt.Errorf(
			"acp: CreateElicitationParams.Mode is %T, which is not a mode this package can send; "+
				"use *ElicitationFormMode or *ElicitationURLMode", mode)
	}
}

// The wire spellings of the two modes, which appear in a capability path, a
// refusal and a construction check and must be the same string in all three.
const (
	elicitationModeForm = "form"
	elicitationModeURL  = "url"
)

var errScopeIsTheOperations = errors.New(
	"acp: the Value of an elicitation mode is its scope, which the operation sets; " +
		"leave it nil and choose the scope by calling AgentSession.CreateElicitation " +
		"or AgentConn.CreateElicitation")

// CompleteElicitation says a URL elicitation is finished and the client may stop
// showing it.
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
	// Closed before the write: the identifier is free for reuse from the moment
	// this side decides the page is finished, and a completion for something never
	// opened is a caller naming an elicitation that does not exist.
	if !c.elicitations.close(params.ElicitationID) {
		return fmt.Errorf(
			"acp: elicitation %q is not outstanding on this connection", params.ElicitationID)
	}
	if err := c.notify(ctx, methodElicitationComplete, params); err != nil {
		c.elicitations.forget(params.ElicitationID)
		return err
	}
	return nil
}

// createElicitation routes an inbound request to the handler for its mode.
func (c *ClientConn) createElicitation(ctx context.Context, request *jsonrpc.Request) (any, error) {
	handlers := c.client.config.Elicitation
	if handlers == nil {
		return nil, methodNotImplemented(request.Method)
	}
	params, err := decodeParams[CreateElicitationRequest](request)
	if err != nil {
		return nil, err
	}

	var response *CreateElicitationResponse
	switch mode := params.Value.(type) {
	case *ElicitationFormMode:
		if handlers.Form == nil {
			return nil, unadvertisedMode(elicitationModeForm, "clientCapabilities.elicitation.form")
		}
		response, err = handlers.Form(ctx, params, mode)

	case *ElicitationURLMode:
		if handlers.URL == nil {
			return nil, unadvertisedMode(elicitationModeURL, "clientCapabilities.elicitation.url")
		}
		// Recorded before the handler runs, so that a completion is recognised
		// however long the user spends on the page. Refusing when the bound is
		// reached is the honest answer: answering without recording would leave
		// this client ignoring the completion for a page it really is showing.
		if !c.elicitations.accept(mode.ElicitationID) {
			return nil, newError(ErrorCodeInternalError,
				"acp: too many URL elicitations are outstanding on this connection")
		}
		response, err = handlers.URL(ctx, params, mode)
		if err != nil {
			c.elicitations.forget(mode.ElicitationID)
		}

	default:
		// The catch-all arm included: guessing which known mode a future one
		// resembles would be worse than saying it is not implemented.
		return nil, newError(ErrorCodeInvalidParams,
			"the elicitation names a mode this package does not implement")
	}

	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, nilHandlerResponse(request.Method)
	}
	return response, nil
}

// completeElicitation clears a URL elicitation and tells the application, in that
// order.
//
// "Clients MUST ignore notifications referencing unknown or already-completed
// IDs", and ignoring is the connection's to do rather than the application's: an
// application cannot tell a completion for a page it never showed from one it has
// already closed, because it does not hold the set. A handler that is not set
// changes nothing here — the elicitation is still closed, because the protocol
// makes hearing about it optional and forgetting about it is not.
func (c *ClientConn) completeElicitation(ctx context.Context, request *jsonrpc.Request) error {
	params, err := decodeParams[CompleteElicitationNotification](request)
	if err != nil {
		return err
	}
	if !c.elicitations.close(params.ElicitationID) {
		c.logger.Debug("acp: ignoring a completion for an unknown or finished elicitation",
			slog.String("elicitationId", string(params.ElicitationID)))
		return nil
	}
	if handlers := c.client.config.Elicitation; handlers != nil && handlers.Complete != nil {
		handlers.Complete(ctx, params)
	}
	return nil
}

func unadvertisedMode(mode, capability string) *Error {
	return newError(ErrorCodeInvalidParams,
		"a %q elicitation was not advertised because %s is not set", mode, capability)
}

// A mode is a parameter capability and not a method one: `elicitation` says the
// client serves the method, and the modes under it say which shapes it can render,
// so a mode it did not advertise is work it cannot do rather than authority it
// withheld. See [PeerInfo.permitsPromptContent] for why that decides the direction.
func (p PeerInfo) permitsElicitationMode(mode CreateElicitationRequestValue) error {
	elicitation, advertised := p.ClientCapabilities.Elicitation.Get()
	if !advertised {
		// Unreachable: the method gate refuses this first, and reaching it would
		// mean the two disagree.
		return unadvertisedMode("", "clientCapabilities.elicitation")
	}
	switch mode.(type) {
	case *ElicitationFormMode:
		if !hasCapability(elicitation.Form) {
			return unadvertisedMode(elicitationModeForm, "clientCapabilities.elicitation.form")
		}
	case *ElicitationURLMode:
		if !hasCapability(elicitation.URL) {
			return unadvertisedMode(elicitationModeURL, "clientCapabilities.elicitation.url")
		}
	}
	return nil
}

// The request a handler is serving travels on the context rather than on a handle
// because there is no handle for it: the only thing identifying an inbound call is
// an identifier this API does not hand out. The connection knows it already — it
// has to, to answer — so the context is where a handler reaches it without anybody
// spelling it.
type servingRequestKey struct{}

func withServingRequest(ctx context.Context, id jsonrpc.ID) context.Context {
	return context.WithValue(ctx, servingRequestKey{}, requestIDOf(id))
}

func servingRequest(ctx context.Context) (requestID, bool) {
	id, serving := ctx.Value(servingRequestKey{}).(requestID)
	return id, serving
}

// urlElicitations is the set of URL elicitations one connection has open.
//
// The protocol puts the two halves of one identifier's lifetime on opposite
// sides. An agent "MUST keep each elicitationId unique among outstanding URL
// elicitations on that Agent-Client connection", and a client "MUST ignore
// notifications referencing unknown or already-completed IDs". Neither side's
// application can keep its half: an agent's handler does not see what other
// handlers on the same connection have open, and a client's cannot know whether an
// identifier was ever shown to the user. The connection sees both, so it owns
// both.
//
// A URL elicitation is outstanding from the moment it is sent until a completion
// clears it. That is longer than the request that created it — the response says
// the page was shown, not that it is finished — which is why this is a set of its
// own rather than something the request bookkeeping already holds.
type urlElicitations struct {
	mu          sync.Mutex
	outstanding map[ElicitationID]struct{}
	limit       int
}

// open claims an identifier for an agent about to send one. The refusal is the
// uniqueness rule: a second elicitation under a live identifier would make the
// completion that follows ambiguous, and the client is entitled to treat one
// identifier as one page.
func (u *urlElicitations) open(id ElicitationID) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, live := u.outstanding[id]; live {
		return fmt.Errorf(
			"acp: elicitation %q is already outstanding on this connection; an identifier is "+
				"unique among the URL elicitations a connection has open", id)
	}
	if len(u.outstanding) >= u.limit {
		return fmt.Errorf("%w (limit %d)", errTooManyElicitations, u.limit)
	}
	if u.outstanding == nil {
		u.outstanding = make(map[ElicitationID]struct{})
	}
	u.outstanding[id] = struct{}{}
	return nil
}

// accept records what a client has been asked to show. It reports false when the
// bound is reached, which the caller turns into a refusal rather than a silent
// omission: a client that answered without recording would then ignore the
// completion for a page it really is showing.
func (u *urlElicitations) accept(id ElicitationID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, live := u.outstanding[id]; live {
		// A peer reusing a live identifier is the agent breaking its own rule.
		// Answering is still correct — the client has one page under that name —
		// so the record stands and this is not an error.
		return true
	}
	if len(u.outstanding) >= u.limit {
		return false
	}
	if u.outstanding == nil {
		u.outstanding = make(map[ElicitationID]struct{})
	}
	u.outstanding[id] = struct{}{}
	return true
}

// close reports whether the identifier was outstanding, and clears it. False is
// the client's "unknown or already-completed", and the agent's "you are completing
// something you never opened".
func (u *urlElicitations) close(id ElicitationID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, live := u.outstanding[id]; !live {
		return false
	}
	delete(u.outstanding, id)
	return true
}

func (u *urlElicitations) forget(id ElicitationID) {
	u.mu.Lock()
	delete(u.outstanding, id)
	u.mu.Unlock()
}
