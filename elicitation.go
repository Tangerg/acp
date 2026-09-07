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
func (e *ElicitationHandlers) check() error {
	if e == nil {
		return nil
	}
	if e.Form == nil && e.URL == nil {
		return errors.New(
			"acp: ElicitationHandlers serves neither mode; set Form, URL, or both")
	}
	if e.URL == nil && e.Complete != nil {
		return errors.New(
			"acp: ElicitationHandlers.Complete is set without URL, so it could never run")
	}
	return nil
}

// A mode with no handler is left absent rather than set false: the schema's
// capability objects use a present `{}` as the only yes.
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

// CreateElicitationParams is an elicitation without its scope, because the
// operation supplies that: a session handle elicits within its session and a
// connection within the request it is serving. This is the division every handle
// here makes — a caller never spells a session identifier either — and it is what
// keeps the request scope unexported, since the scope it names is a JSON-RPC
// request identifier this API does not hand out.
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

// The mode is copied before the scope is written into it: the value is the
// caller's, and an agent holding one prepared schema and eliciting with it twice
// would otherwise find the second call carrying the first call's scope.
func (c *AgentConn) createElicitation(
	ctx context.Context,
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
	if err := c.awaitHandshake(ctx, methodElicitationCreate); err != nil {
		return nil, err
	}
	if err := c.Peer().permits(methodElicitationCreate); err != nil {
		return nil, err
	}
	if err := c.Peer().permitsElicitationMode(mode); err != nil {
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
		reservation, err := c.elicitations.reserve(url.ElicitationID, c.limits.OutstandingElicitations)
		if err != nil {
			return nil, err
		}
		call, err := c.send(ctx, methodElicitationCreate, request, func(answer *jsonrpc.Response) error {
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
		if awaitErr := c.await(ctx, call); awaitErr != nil {
			return nil, awaitErr
		}
		return response, nil
	}

	if err := c.call(ctx, methodElicitationCreate, request, response); err != nil {
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

// An elicitationMode is one of the two shapes a client can render: its wire
// spelling, and the capability that advertises it.
//
// The two are one value because they are one fact stated twice. Every refusal
// names both, the gate table gates on the path, and construction checks the
// advertisement against the handler — so a spelling that lived apart from its
// path could report one mode while gating another.
type elicitationMode struct {
	parameterCapability
	// advertised reads the one field under the elicitation capability that says
	// whether this mode is offered.
	advertised func(ElicitationCapabilities) bool
}

// The wire spellings. The generated codec emits the same two strings as the
// union's discriminant, so a refusal naming a different one would name a mode no
// peer can have sent.
const (
	elicitationModeForm = "form"
	elicitationModeURL  = "url"

	elicitationCapability = "clientCapabilities.elicitation"
)

var (
	elicitationForm = elicitationMode{
		parameterCapability: elicitationOf(elicitationModeForm),
		advertised:          func(c ElicitationCapabilities) bool { return hasCapability(c.Form) },
	}
	elicitationURL = elicitationMode{
		parameterCapability: elicitationOf(elicitationModeURL),
		advertised:          func(c ElicitationCapabilities) bool { return hasCapability(c.URL) },
	}
)

// An absent elicitation object advertises no mode, so the zero value answers
// without a branch of its own.
func (m elicitationMode) advertisedBy(client ClientCapabilities) bool {
	elicitation, _ := client.Elicitation.Get()
	return m.advertised(elicitation)
}

// elicitationModeOf names the wire arm, so that the capability check and the
// refusal it produces read the same value rather than two parallel switches.
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

func elicitationOf(mode string) parameterCapability {
	return parameterCapability{
		kind:       mode,
		noun:       "elicitation",
		capability: elicitationCapability + "." + mode,
	}
}

var errScopeIsTheOperations = errors.New(
	"acp: the Value of an elicitation mode is its scope, which the operation sets; " +
		"leave it nil and choose the scope by calling AgentSession.CreateElicitation " +
		"or AgentConn.CreateElicitation")

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
