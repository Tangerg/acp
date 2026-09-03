package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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

	// Returning accept from URL consents to start the out-of-band interaction and
	// keeps its ID outstanding until the agent sends a completion. Every other
	// action releases the ID with the response.
	URL func(
		ctx context.Context,
		request *CreateElicitationRequest,
		mode *ElicitationURLMode,
	) (*CreateElicitationResponse, error)

	// Complete is optional even when URL is set, because the protocol makes
	// sending one optional: an agent "MAY send elicitation/complete". A client
	// that leaves it nil still tracks each accepted interaction and ignores a
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
		id, method, serving := servingRequest(ctx)
		if !serving {
			return nil, errors.New(
				"acp: AgentConn.CreateElicitation is scoped to the request being served, so it " +
					"must be called from a handler with the context that handler was given")
		}
		// The schema defines this scope as "tied to a specific JSON-RPC request
		// outside of a session". A session-scoped request has a session, so an
		// elicitation from one belongs to that session and AgentSession is where it
		// is asked for; scoping it to the request instead would name a call the
		// client already knows belongs to a conversation.
		if descriptor, standard := standardMethods[method]; standard {
			if descriptor.side != sideAgent {
				return nil, fmt.Errorf(
					"acp: %q is not a request this connection serves, so it cannot be an elicitation's scope",
					method)
			}
			if descriptor.requiresSessionID {
				return nil, fmt.Errorf(
					"acp: %q is scoped to a session because its params require a session ID; "+
						"use AgentSession.CreateElicitation", method)
			}
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
	if url, isURL := scoped.(*ElicitationURLMode); isURL {
		reservation, err := conn.elicitations.reserve(url.ElicitationID)
		if err != nil {
			return nil, err
		}
		call, err := conn.send(ctx, methodElicitationCreate, request, func(answer *jsonrpc.Response) error {
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
		if err := conn.await(ctx, call); err != nil {
			return nil, err
		}
		return response, nil
	}

	if err := conn.call(ctx, methodElicitationCreate, request, response); err != nil {
		return nil, err
	}
	if form, isForm := mode.(*ElicitationFormMode); isForm {
		if content, answered := response.acceptedContent(); answered {
			if bad := form.RequestedSchema.validate(content); bad != nil {
				return nil, bad
			}
		}
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

// CompleteElicitation says an accepted URL interaction is finished and the
// client may stop presenting it.
//
// Calling it for an ID whose create response did not accept returns an error.
// When the transport proves a failed send committed no bytes, the ID remains
// outstanding so the caller can retry.
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
	completion, outstanding := c.elicitations.beginCompletion(params.ElicitationID)
	if !outstanding {
		return fmt.Errorf(
			"acp: elicitation %q is not outstanding on this connection", params.ElicitationID)
	}
	if err := c.notify(ctx, methodElicitationComplete, params); err != nil {
		// The transport promises that an exact context error means it committed no
		// bytes. writeFailure keeps the connection alive only in that case, so a
		// live connection means retrying this completion is both safe and useful.
		if !c.ended() {
			completion.unsent()
		}
		return err
	}
	completion.sent()
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
		if err == nil {
			if content, answered := response.acceptedContent(); answered {
				if bad := mode.RequestedSchema.validate(content); bad != nil {
					// The client's own form and the client's own answer disagree, so
					// this is a bug on this side and the agent is told so rather than
					// handed a value its schema does not describe.
					return nil, newError(ErrorCodeInternalError, "%s", bad.Error())
				}
			}
		}

	case *ElicitationURLMode:
		if handlers.URL == nil {
			return nil, unadvertisedMode(elicitationModeURL, "clientCapabilities.elicitation.url")
		}
		reservation, reserveErr := c.elicitations.reserve(mode.ElicitationID)
		if reserveErr != nil {
			if errors.Is(reserveErr, errElicitationIDInUse) {
				return nil, newError(ErrorCodeInvalidParams, "%s", reserveErr)
			}
			return nil, newError(ErrorCodeInternalError, "%s", reserveErr)
		}
		response, err = handlers.URL(ctx, params, mode)
		if err != nil || response == nil || !response.accepted() {
			reservation.reject()
		} else {
			// The protocol state commits only to an accept the peer can receive.
			// Response encoding normally happens after the handler returns; checking
			// it here prevents a malformed union supplied by the handler from
			// leaving an interaction outstanding after the peer received -32603.
			if _, encodeErr := json.Marshal(response); encodeErr != nil {
				reservation.reject()
				err = fmt.Errorf("acp: URL elicitation response cannot be encoded: %w", encodeErr)
			} else {
				reservation.accept()
			}
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
// application cannot tell a completion for an interaction it never accepted from
// one already closed, because it does not hold the set. A handler that is not set
// changes nothing here — the elicitation is still closed, because the protocol
// makes hearing about it optional and forgetting about it is not.
func (c *ClientConn) completeElicitation(ctx context.Context, request *jsonrpc.Request) error {
	params, err := decodeParams[CompleteElicitationNotification](request)
	if err != nil {
		return err
	}
	if !c.elicitations.receiveCompletion(params.ElicitationID) {
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

// servedRequest is the identity of the call a handler is answering: which one,
// and which method. The method is there because the identifier alone does not say
// whether the request has a session, and a request-scoped elicitation is defined
// as one outside a session.
type servedRequest struct {
	id     requestID
	method string
}

func withServingRequest(ctx context.Context, id jsonrpc.ID, method string) context.Context {
	return context.WithValue(ctx, servingRequestKey{}, servedRequest{requestIDOf(id), method})
}

func servingRequest(ctx context.Context) (requestID, string, bool) {
	served, serving := ctx.Value(servingRequestKey{}).(servedRequest)
	return served.id, served.method, serving
}
