package acp

import (
	"context"
	"errors"
	"fmt"

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

	// Complete is required when URL is set and refused otherwise. A URL
	// elicitation outlives its own response — the response says the page is
	// shown, this says it is finished — so a client that serves the mode without
	// this leaves the user in front of a dialog nothing will ever close.
	Complete func(ctx context.Context, notification *CompleteElicitationNotification)
}

// check refuses at construction what would otherwise be found by a user: a group
// that advertises elicitation and serves no mode, and a page nothing will close.
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

// The gate has already refused the method for a client with no group, so the nil
// case is unreachable — and a dispatcher that would dereference it if it ever were
// is a panic resting on an invariant held in another file.
func (handlers *ElicitationHandlers) completion() func(context.Context, *CompleteElicitationNotification) {
	if handlers == nil {
		return nil
	}
	return handlers.Complete
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

// The catch-all arm exists so a mode this package does not know survives being
// decoded from a peer. Sending one is the opposite: a discriminant this side made
// up.
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
	return c.notify(ctx, methodElicitationComplete, params)
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
			return nil, unadvertisedMode("form", "clientCapabilities.elicitation.form")
		}
		response, err = handlers.Form(ctx, params, mode)

	case *ElicitationURLMode:
		if handlers.URL == nil {
			return nil, unadvertisedMode("url", "clientCapabilities.elicitation.url")
		}
		response, err = handlers.URL(ctx, params, mode)

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
			return unadvertisedMode("form", "clientCapabilities.elicitation.form")
		}
	case *ElicitationURLMode:
		if !hasCapability(elicitation.URL) {
			return unadvertisedMode("url", "clientCapabilities.elicitation.url")
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
