package acp

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tangerg/acp/jsonrpc"
)

// ElicitationHandlers is a client's ability to put an elicitation in front of the
// user. The two modes are two independent capabilities, so they are two fields,
// and each advertises itself. Both arrive through one method, so a mode with no
// handler is refused with invalid-params: the method was advertised, the mode
// inside it was not.
//
// A handler receives the request and its Value, already asserted to the mode
// that chose the handler.
type ElicitationHandlers struct {
	Form func(
		ctx context.Context,
		conn *ClientConn,
		request *CreateElicitationRequest,
		mode *ElicitationFormMode,
	) (*CreateElicitationResponse, error)

	// Returning accept from URL consents to start the out-of-band interaction
	// and keeps its ID outstanding until the agent sends a completion; every
	// other action releases the ID.
	URL func(
		ctx context.Context,
		conn *ClientConn,
		request *CreateElicitationRequest,
		mode *ElicitationURLMode,
	) (*CreateElicitationResponse, error)

	// Complete is optional even with URL set, because an agent "MAY send
	// elicitation/complete". Without it the client still tracks accepted
	// interactions and ignores completions it does not recognise.
	Complete func(ctx context.Context, conn *ClientConn, notification *CompleteElicitationNotification)
}

// check refuses a group that renders no mode, and a completion handler no mode
// could reach.
func (e *ElicitationHandlers) check() error {
	if e == nil {
		return nil
	}
	if e.Form == nil && e.URL == nil {
		return errors.New("acp: ElicitationHandlers serves neither mode; set Form, URL, or both")
	}
	if e.URL == nil && e.Complete != nil {
		return errors.New("acp: ElicitationHandlers.Complete is set without URL, so it could never run")
	}
	return nil
}

// capabilities derives the advertisement. A mode without a handler is absent
// rather than false: a present `{}` is the only yes.
func (e *ElicitationHandlers) capabilities() Opt[ElicitationCapabilities] {
	if e == nil {
		return Opt[ElicitationCapabilities]{}
	}
	var advertised ElicitationCapabilities
	if e.Form != nil {
		advertised.Form = OptValue(ElicitationFormCapabilities{})
	}
	if e.URL != nil {
		advertised.URL = OptValue(ElicitationURLCapabilities{})
	}
	return OptValue(advertised)
}

// unserved reports the modes a stated advertisement claims that no handler
// renders. Advertising fewer modes than the handlers serve is the client's
// choice.
func (e *ElicitationHandlers) unserved(stated ClientCapabilities) []string {
	var unserved []string
	for _, mode := range []struct {
		elicitationMode
		served bool
	}{
		{elicitationForm, e != nil && e.Form != nil},
		{elicitationURL, e != nil && e.URL != nil},
	} {
		if mode.advertisedIn(stated) && !mode.served {
			unserved = append(unserved, mode.capability+" advertises a mode with no handler that renders it")
		}
	}
	return unserved
}

// CreateElicitationParams is an elicitation without its scope, which the
// operation supplies: a session handle elicits within its session, a connection
// within the request it is serving.
type CreateElicitationParams struct {
	// Message is shown to the user.
	Message string

	// Mode is *[ElicitationFormMode] or *[ElicitationURLMode], with its Value
	// left nil: the Value is the scope, and the operation sets it.
	Mode CreateElicitationRequestValue

	// ToolCallID is for session scope only, as an agent relaying an MCP server's
	// elicitation during a tool call needs. [AgentConn.CreateElicitation] refuses
	// it rather than dropping it.
	ToolCallID Opt[ToolCallID]

	Meta Opt[Meta]
}

// An elicitationScope writes the operation's scope into a mode.
type elicitationScope interface {
	apply(mode CreateElicitationRequestValue, params *CreateElicitationParams)
}

type sessionScope struct {
	session SessionID
}

func (s sessionScope) apply(mode CreateElicitationRequestValue, params *CreateElicitationParams) {
	scope := ElicitationSessionScope{SessionID: s.session, ToolCallID: params.ToolCallID}
	switch mode := mode.(type) {
	case *ElicitationFormMode:
		mode.Value = &ElicitationFormModeSession{ElicitationSessionScope: scope}
	case *ElicitationURLMode:
		mode.Value = &ElicitationURLModeSession{ElicitationSessionScope: scope}
	}
}

type requestScope struct {
	request jsonrpc.ID
}

func (s requestScope) apply(mode CreateElicitationRequestValue, _ *CreateElicitationParams) {
	scope := ElicitationRequestScope{RequestID: s.request}
	switch mode := mode.(type) {
	case *ElicitationFormMode:
		mode.Value = &ElicitationFormModeRequest{ElicitationRequestScope: scope}
	case *ElicitationURLMode:
		mode.Value = &ElicitationURLModeRequest{ElicitationRequestScope: scope}
	}
}

// createElicitation sends an elicitation in a scope. The mode is copied before
// the scope is written into it, because it is the caller's: an agent eliciting
// twice with one prepared schema would otherwise send the first call's scope.
func (c *AgentConn) createElicitation(
	ctx context.Context,
	params *CreateElicitationParams,
	scope elicitationScope,
	parent *inboundRequest,
) (*CreateElicitationResponse, error) {
	if params == nil {
		params = new(CreateElicitationParams)
	}
	mode, err := checkedMode(params.Mode)
	if err != nil {
		return nil, err
	}
	scoped := deepCopy(mode)
	scope.apply(scoped, params)
	var form *compiledForm
	if formMode, isForm := scoped.(*ElicitationFormMode); isForm {
		if form, err = newCompiledForm(formMode.RequestedSchema); err != nil {
			return nil, err
		}
	}
	request := &CreateElicitationRequest{Message: params.Message, Meta: params.Meta, Value: scoped}

	if url, isURL := scoped.(*ElicitationURLMode); isURL {
		return c.createURLElicitation(ctx, request, url.ElicitationID, parent)
	}
	call, err := c.sendElicitation(ctx, request, parent, nil, nil)
	if err != nil {
		return nil, err
	}
	response := new(CreateElicitationResponse)
	if awaitErr := c.awaitInto(ctx, call, response); awaitErr != nil {
		return nil, awaitErr
	}
	if bad := form.validateResponse(ctx, response); bad != nil {
		return nil, bad
	}
	return response, nil
}

// createURLElicitation reserves the ID before the request can reach the wire.
// The response, even one arriving after the caller left, alone decides whether
// the interaction became outstanding.
func (c *AgentConn) createURLElicitation(
	ctx context.Context,
	request *CreateElicitationRequest,
	id ElicitationID,
	parent *inboundRequest,
) (*CreateElicitationResponse, error) {
	reservation, err := c.elicitations.reserve(id)
	if err != nil {
		return nil, err
	}
	response := new(CreateElicitationResponse)
	call, err := c.sendElicitation(ctx, request, parent, func(answer *jsonrpc.Response) error {
		if decodeErr := decodeResponse(answer, response); decodeErr != nil {
			reservation.reject()
			return decodeErr
		}
		if response.accepted() {
			reservation.accept()
		} else {
			reservation.reject()
		}
		return nil
	}, reservation.reject)
	if err != nil {
		reservation.reject()
		return nil, err
	}
	if _, awaitErr := c.await(ctx, call); awaitErr != nil {
		return nil, awaitErr
	}
	return response, nil
}

// sendElicitation holds a request-scoped elicitation's parent send gate during
// the write, so the write commits before the parent's answer.
func (c *AgentConn) sendElicitation(
	ctx context.Context,
	request *CreateElicitationRequest,
	parent *inboundRequest,
	accept func(*jsonrpc.Response) error,
	abandon func(),
) (outboundCall, error) {
	if parent != nil {
		if err := parent.beginSending(ctx); err != nil {
			return outboundCall{}, err
		}
		defer parent.finishSending()
	}
	return c.sendStandard(ctx, methodElicitationCreate, request, accept, abandon)
}

// checkedMode refuses what this side cannot send. The catch-all arm exists so a
// mode this package does not know survives decoding; sending one would be a
// discriminant this side made up.
func checkedMode(mode CreateElicitationRequestValue) (CreateElicitationRequestValue, error) {
	switch mode := mode.(type) {
	case *ElicitationFormMode:
		if mode == nil {
			break
		}
		if mode.Value != nil {
			return nil, errScopeIsTheOperations
		}
		return mode, nil
	case *ElicitationURLMode:
		if mode == nil {
			break
		}
		if mode.Value != nil {
			return nil, errScopeIsTheOperations
		}
		return mode, nil
	case nil:
	default:
		return nil, fmt.Errorf("acp: CreateElicitationParams.Mode is %T, which is not a mode this package can "+
			"send; use *ElicitationFormMode or *ElicitationURLMode", mode)
	}
	return nil, errors.New("acp: CreateElicitationParams.Mode is required")
}

var errScopeIsTheOperations = errors.New(
	"acp: the Value of an elicitation mode is its scope, which the operation sets; leave it nil and choose " +
		"the scope by calling AgentSession.CreateElicitation or AgentConn.CreateElicitation")

// An elicitationMode is one of the two shapes a client can render, as the
// parameter capability that advertises it.
type elicitationMode struct {
	parameterCapability
	// leaf is the capability's path under clientCapabilities.
	leaf string
	get  func(ElicitationCapabilities) bool
}

// advertisedIn reads the mode from a client advertisement; an absent elicitation
// object advertises no mode.
func (m elicitationMode) advertisedIn(client ClientCapabilities) bool {
	elicitation, _ := client.Elicitation.Get()
	return m.get(elicitation)
}

func newElicitationMode(tag, leaf string, get func(ElicitationCapabilities) bool) elicitationMode {
	mode := elicitationMode{leaf: leaf, get: get}
	mode.parameterCapability = parameterCapability{
		value:      fmt.Sprintf("a %q elicitation", tag),
		capability: "clientCapabilities." + leaf,
		advertised: func(n *Negotiated) bool { return mode.advertisedIn(n.ClientCapabilities) },
	}
	return mode
}

var (
	elicitationForm = newElicitationMode(elicitationFormModeTag, "elicitation.form",
		func(c ElicitationCapabilities) bool { return hasCapability(c.Form) })
	elicitationURL = newElicitationMode(elicitationURLModeTag, "elicitation.url",
		func(c ElicitationCapabilities) bool { return hasCapability(c.URL) })
)

// anyElicitationMode reports whether a client can render anything. The RFD is
// explicit that a present elicitation object with no mode advertises no modes,
// so it does not advertise the method either.
func anyElicitationMode(client ClientCapabilities) bool {
	return elicitationForm.advertisedIn(client) || elicitationURL.advertisedIn(client)
}

func elicitationModeOf(value CreateElicitationRequestValue) (elicitationMode, bool) {
	switch value.(type) {
	case *ElicitationFormMode:
		return elicitationForm, true
	case *ElicitationURLMode:
		return elicitationURL, true
	default:
		return elicitationMode{}, false
	}
}

// registerElicitation reserves a URL elicitation's ID in wire order, before its
// handler can show it, and commits the reservation only when an accepting answer
// reaches the peer.
func registerElicitation(handlers ElicitationHandlers) func(*ClientConn, *inboundRequest) registration {
	return func(c *ClientConn, call *inboundRequest) registration {
		request := paramsOf[CreateElicitationRequest](call)
		url, isURL := request.Value.(*ElicitationURLMode)
		if !isURL || url == nil || handlers.URL == nil {
			return dispatched(callHooks{})
		}
		reservation, err := c.elicitations.reserve(url.ElicitationID)
		if errors.Is(err, errElicitationIDInUse) {
			return answered(nil, newError(ErrorCodeInvalidParams, "%s", err))
		}
		if err != nil {
			return answered(nil, err)
		}
		return dispatched(callHooks{finished: func(written any) {
			if response, ok := written.(*CreateElicitationResponse); ok && response.accepted() {
				reservation.accept()
				return
			}
			reservation.reject()
		}})
	}
}

// serveElicitation routes an elicitation to the handler for its mode. A form is
// compiled before the handler sees it and the handler's answer is held to it.
func serveElicitation(
	handlers ElicitationHandlers,
) func(*ClientConn, context.Context, *inboundRequest, *CreateElicitationRequest) (*CreateElicitationResponse, error) {
	return func(c *ClientConn, ctx context.Context, _ *inboundRequest, request *CreateElicitationRequest) (
		*CreateElicitationResponse, error,
	) {
		switch mode := request.Value.(type) {
		case *ElicitationFormMode:
			if handlers.Form == nil {
				return nil, elicitationForm.refusedInbound()
			}
			form, err := newCompiledForm(mode.RequestedSchema)
			if err != nil {
				return nil, newError(ErrorCodeInvalidParams, "%s", err)
			}
			response, err := handlers.Form(ctx, c, request, mode)
			if err != nil || response == nil {
				return response, err
			}
			if bad := form.validateResponse(ctx, response); bad != nil {
				// The answer violates the peer's valid schema, so the fault is this
				// handler's rather than the request's.
				return nil, fmt.Errorf("acp: the form handler's answer is invalid: %w", bad)
			}
			return response, nil
		case *ElicitationURLMode:
			if handlers.URL == nil {
				return nil, elicitationURL.refusedInbound()
			}
			return handlers.URL(ctx, c, request, mode)
		default:
			// The catch-all included: guessing which known mode a future one
			// resembles would be worse than saying it is not implemented.
			return nil, newError(ErrorCodeInvalidParams, "the elicitation names a mode this package does not implement")
		}
	}
}

// serveCompletion clears a URL elicitation and then tells the application.
// "Clients MUST ignore notifications referencing unknown or already-completed
// IDs", and only the connection holds the set to tell them apart.
func serveCompletion(
	complete func(context.Context, *ClientConn, *CompleteElicitationNotification),
) func(*ClientConn, context.Context, *CompleteElicitationNotification) {
	return func(c *ClientConn, ctx context.Context, notification *CompleteElicitationNotification) {
		if c.elicitations.receiveCompletion(notification.ElicitationID) && complete != nil {
			complete(ctx, c, notification)
		}
	}
}

// The request a handler is serving travels on its context, because no handle
// names an inbound call: its identifier is not handed out. The connection knows
// it already, to answer it.
type servingRequestKey struct{}

func withServingRequest(ctx context.Context, request *inboundRequest) context.Context {
	return context.WithValue(ctx, servingRequestKey{}, request)
}

func servingRequest(ctx context.Context) (*inboundRequest, bool) {
	request, serving := ctx.Value(servingRequestKey{}).(*inboundRequest)
	return request, serving
}
