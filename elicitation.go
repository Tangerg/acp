package acp

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/acp/jsonrpc"
)

// Elicitation is the agent asking the user for structured input, through the
// client that owns the user.
//
// It is one feature and not two methods. `elicitation/create` carries a mode and
// a scope, and both are unions the schema flattens into the same JSON object:
// form mode hands the client a JSON Schema to render, URL mode sends the user to
// a page and is answered twice — once by the response, and again later by
// `elicitation/complete` when the page is done with them.
//
// # Who names what
//
// The mode is the caller's and the scope is the operation's, which is the same
// division every other handle in this package makes. A [ClientSession] never
// spells a session identifier and a [TerminalHandle] never spells a terminal
// identifier; here an agent never spells a scope, because the operation it chose
// has already decided one. [AgentSession.CreateElicitation] elicits within the
// session it names. [AgentConn.CreateElicitation] elicits within the request it
// is being called from, which is what the schema means by a request-scoped
// elicitation: the phases before any session exists, an agent asking for
// something from inside its own authenticate handler.
//
// That second one is why `ElicitationRequestScope` is not exported. It names a
// JSON-RPC request identifier, and this API does not surface those — a caller
// names a method by calling the operation for it, and the same rule applies to
// the request that operation is answering. The connection already knows which
// request a handler is serving, so the scope is filled in from the context the
// handler was given, and the arms that carry it are exported types wrapping an
// unexported one. A client can tell a request-scoped elicitation from a
// session-scoped one by its arm; neither side hands a caller the identifier.

// ElicitationHandlers is a client's ability to put an elicitation in front of the
// user. Setting one advertises the mode it serves.
//
// The two modes are two independent capabilities — `elicitation.form` and
// `elicitation.url` — the way the two file-system booleans are, so they are two
// handler fields and not one. Both arrive through `elicitation/create`, so the
// dispatcher routes on the mode the request carries and a mode with no handler is
// refused with invalid-params: the method was advertised, and it is the mode
// inside it that this client never said it could render.
//
// At least one is required, because a client advertising elicitation and serving
// neither mode has advertised nothing it can do.
type ElicitationHandlers struct {
	// Form renders the request's JSON Schema and answers with what the user
	// entered. Setting it advertises clientCapabilities.elicitation.form.
	//
	// The mode is passed alongside the request it came from. It is the request's
	// own Value, already asserted: the dispatcher had to look at it to choose this
	// handler, and making the handler repeat the assertion would be asking it to
	// re-derive a decision that has just been made.
	Form func(
		ctx context.Context,
		request *CreateElicitationRequest,
		mode *ElicitationFormMode,
	) (*CreateElicitationResponse, error)

	// URL directs the user to the mode's URL. Setting it advertises
	// clientCapabilities.elicitation.url.
	URL func(
		ctx context.Context,
		request *CreateElicitationRequest,
		mode *ElicitationURLMode,
	) (*CreateElicitationResponse, error)

	// Complete says a URL elicitation has been answered where the user went, and
	// is required when URL is set.
	//
	// A URL elicitation is the one exchange in the protocol that outlives its own
	// response: the client answers `elicitation/create` to say it has shown the
	// user the page, and the agent sends this later to say the page is finished.
	// A client that shows a page and is never told to stop showing it has a
	// dialog nothing will ever close, so the mode is not servable without this
	// and construction says so rather than leaving the user with it.
	//
	// It is refused when URL is not set. Serving the completion of a mode this
	// client cannot start is not a partial implementation, it is a handler that
	// can never run.
	Complete func(ctx context.Context, notification *CompleteElicitationNotification)
}

// check refuses a handler set that could not serve what it advertises. Both rules
// are about the user rather than about the wire: a client with no mode has
// advertised nothing, and a URL mode with no completion leaves a page open.
func (handlers *ElicitationHandlers) check() error {
	if handlers == nil {
		return nil
	}
	if handlers.Form == nil && handlers.URL == nil {
		return errors.New(
			"acp: ElicitationHandlers serves neither mode; set Form, URL, or both")
	}
	if handlers.URL != nil && handlers.Complete == nil {
		return errors.New(
			"acp: ElicitationHandlers.URL is set without Complete, so a client would show " +
				"the user a page and never be told it is finished")
	}
	if handlers.URL == nil && handlers.Complete != nil {
		return errors.New(
			"acp: ElicitationHandlers.Complete is set without URL, so it could never run")
	}
	return nil
}

// completion reads Complete through a possibly nil group.
//
// The capability gate has already refused elicitation/complete for a client that
// advertised no url mode, so this cannot be reached with a nil group — but a
// dispatcher that would dereference one if it ever were is a panic waiting on an
// invariant held somewhere else, and the handler it returns is checked for nil by
// the dispatch helper anyway.
func (handlers *ElicitationHandlers) completion() func(context.Context, *CompleteElicitationNotification) {
	if handlers == nil {
		return nil
	}
	return handlers.Complete
}

// capabilities is the advertisement the handlers imply. A mode with no handler is
// absent rather than present-and-false, because the schema's capability objects
// use a present `{}` as the only yes.
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

// CreateElicitationParams is an elicitation without its scope.
//
// The scope is missing because the operation supplies it, and which operation was
// called is what decides it. See the package documentation on this file's feature
// for why a caller never names one.
type CreateElicitationParams struct {
	// Message describes what is being asked for, in the user's language rather
	// than the schema's. Required.
	Message string

	// Mode is *[ElicitationFormMode] or *[ElicitationURLMode]. Required.
	//
	// Its own Value — the scope — must be nil. A scope set here would be replaced
	// by the operation, and replacing a caller's value in silence is worse than
	// refusing it, so it is refused.
	Mode CreateElicitationRequestValue

	// ToolCallID ties a session-scoped elicitation to one tool call, which is what
	// an agent relaying an elicitation from an MCP server has to say. It is
	// meaningful only within a session, so [AgentConn.CreateElicitation] refuses
	// it rather than dropping it.
	ToolCallID Opt[ToolCallID]

	Meta Opt[Meta]
}

// CreateElicitation asks the user for structured input within this session,
// optionally tied to a tool call.
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
// connection is serving, for the phases where there is no session to belong to.
//
// It must be called from within a handler, on the context that handler was given,
// because the scope is the request being served and nothing else identifies one.
// Called anywhere else it fails rather than inventing a scope.
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

// createElicitation is everything the two scopes share, which is everything
// except the scope.
//
// The mode is copied before the scope is written into it. The value belongs to the
// caller, who may well be holding one prepared schema and eliciting with it more
// than once, and an operation that reached into it would make the second call
// carry the first call's scope.
func createElicitation(
	ctx context.Context,
	conn *AgentConn,
	params *CreateElicitationParams,
	scope func(CreateElicitationRequestValue) (CreateElicitationRequestValue, error),
) (*CreateElicitationResponse, error) {
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
	request := &CreateElicitationRequest{
		Message: params.Message,
		Meta:    params.Meta,
		Value:   scoped,
	}
	response := new(CreateElicitationResponse)
	if err := conn.call(ctx, methodElicitationCreate, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

// checkedMode refuses the two modes a caller can name that cannot be sent.
//
// The catch-all arm exists so that a mode this package does not know survives
// being decoded from a peer. Sending one is the opposite: it would put a mode on
// the wire under a discriminant this side made up.
func checkedMode(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
	switch mode := mode.(type) {
	case nil:
		return nil, paramsRequired("CreateElicitation", "Mode")

	case *ElicitationFormMode:
		if mode.Value != nil {
			return nil, errScopeIsTheOperations
		}
		return mode, nil

	case *ElicitationURLMode:
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

var errScopeIsTheOperations = errors.New(
	"acp: the Value of an elicitation mode is its scope, which the operation sets; " +
		"leave it nil and choose the scope by calling AgentSession.CreateElicitation " +
		"or AgentConn.CreateElicitation")

// CompleteElicitation says a URL elicitation is finished and the client may stop
// showing it.
//
// It is on the connection rather than on a session because the notification names
// only an elicitation, and a URL elicitation started before any session exists has
// no session to send it under.
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
	return c.notify(ctx, methodElicitationComplete, params)
}

// createElicitation routes an inbound request to the handler for its mode.
//
// The refusal for a mode with no handler is invalid-params and not
// method-not-found, for the reason the parameter capabilities give: the method is
// there, this client advertised it, and it is what the agent put inside it that
// was never advertised. An agent reading the error is told which capability it
// went past rather than being told the method does not exist, which would be
// false and would send it looking in the wrong place.
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
			return nil, unadvertisedMode("form", "clientCapabilities.elicitation.form")
		}
		response, err = handlers.Form(ctx, params, mode)

	case *ElicitationURLMode:
		if handlers.URL == nil {
			return nil, unadvertisedMode("url", "clientCapabilities.elicitation.url")
		}
		response, err = handlers.URL(ctx, params, mode)

	default:
		// Including the catch-all arm, which is how a mode from a later schema
		// arrives. There is no handler for a mode this package cannot name, and
		// guessing which of the two it resembles would be worse than saying so.
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

func unadvertisedMode(mode, capability string) *Error {
	return newError(ErrorCodeInvalidParams,
		"a %q elicitation was not advertised because %s is not set", mode, capability)
}

// permitsElicitationMode is the outbound half, and it is a parameter capability
// rather than a method one: `elicitation` says the client serves the method and
// the two modes under it say which shapes it can render, so a mode the client did
// not advertise is work it cannot do rather than authority it withheld.
//
// It is checked on the way out for the reason every parameter capability is: the
// client is where the specification puts the obligation to look, and an agent that
// asks anyway learns which capability it went past instead of reading a wire
// trace.
func (p PeerInfo) permitsElicitationMode(mode CreateElicitationRequestValue) error {
	elicitation, advertised := p.ClientCapabilities.Elicitation.Get()
	if !advertised {
		// The method gate has already refused this; reaching here would mean the
		// two disagree.
		return unadvertisedMode("", "clientCapabilities.elicitation")
	}
	switch mode.(type) {
	case *ElicitationFormMode:
		if !hasCapability(elicitation.Form) {
			return unadvertisedMode("form", "clientCapabilities.elicitation.form")
		}
	case *ElicitationURLMode:
		if !hasCapability(elicitation.URL) {
			return unadvertisedMode("url", "clientCapabilities.elicitation.url")
		}
	}
	return nil
}

// The request a handler is serving, carried on the context that handler is given.
//
// It is on the context rather than on a handle because there is no handle for it:
// a request-scoped elicitation belongs to an inbound call, and the only thing that
// identifies one is an identifier this API does not hand out. The connection knows
// it already — it has to, to answer — so the context is where a handler can reach
// it without anybody spelling it.
type servingRequestKey struct{}

func withServingRequest(ctx context.Context, id jsonrpc.ID) context.Context {
	return context.WithValue(ctx, servingRequestKey{}, requestIDOf(id))
}

func servingRequest(ctx context.Context) (requestID, bool) {
	id, serving := ctx.Value(servingRequestKey{}).(requestID)
	return id, serving
}
