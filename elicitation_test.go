package acp_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/acp"
)

// Elicitation, driven from an agent's prompt handler for the session scope and
// from its authenticate handler for the request scope, because those are the two
// places the schema says an elicitation comes from.

func formSchema() acp.ElicitationSchema {
	return acp.ElicitationSchema{
		Type: acp.ElicitationSchemaTypeObject,
		Properties: map[string]acp.ElicitationPropertySchema{
			"branch": &acp.StringPropertySchema{},
		},
	}
}

// The request scope names the call the agent is answering, and the agent never
// spells the identifier: it comes from the context the handler was given.
//
// This is the case the schema describes — an agent eliciting during
// authentication, before any session exists — so it is driven from an
// authenticate handler, which has no session and reaches the client through the
// connection it is given.
func TestARequestScopedElicitationNamesTheRequestBeingServed(t *testing.T) {
	scopes := make(chan acp.ElicitationFormModeValue, 1)

	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				_ context.Context, _ *acp.ClientConn, _ *acp.CreateElicitationRequest,
				mode *acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				scopes <- mode.Value
				return &acp.CreateElicitationResponse{
					Value: &acp.ElicitationAcceptAction{},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var elicitErr error
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(
			context.Context,
			*acp.AgentConn,
			*acp.NewSessionRequest,
		) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: testSessionID}, nil
		},
		Prompt: func(
			context.Context,
			*acp.AgentSession,
			*acp.PromptRequest,
		) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodAgent{ID: "token", Name: "Token"}},

		// No session exists here, and none will until the client is authenticated.
		// The connection is the handle, and the request being answered is the scope.
		Authenticate: func(
			ctx context.Context,
			conn *acp.AgentConn,
			_ *acp.AuthenticateRequest,
		) (*acp.AuthenticateResponse, error) {
			_, elicitErr = conn.CreateElicitation(ctx, &acp.CreateElicitationParams{
				Message: "paste your token",
				Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
			})
			return &acp.AuthenticateResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	conn := connectPeers(t, client, agent)
	if _, err := conn.Authenticate(context.Background(), &acp.AuthenticateRequest{
		MethodID: "token",
	}); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if elicitErr != nil {
		t.Fatalf("CreateElicitation: %v", elicitErr)
	}

	// A request scope rather than a session scope, and the client can tell which
	// without being handed the identifier that distinguishes them.
	if scope := <-scopes; !isRequestScope(scope) {
		t.Fatalf("the scope arrived as %T, want a request scope", scope)
	}
}

func isRequestScope(scope acp.ElicitationFormModeValue) bool {
	_, request := scope.(*acp.ElicitationFormModeRequest)
	return request
}

func TestARequestScopeCannotEscapeItsConnectionOrItsRequest(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{Form: func(
			context.Context, *acp.ClientConn, *acp.CreateElicitationRequest, *acp.ElicitationFormMode,
		) (*acp.CreateElicitationResponse, error) {
			return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	contexts := make(chan context.Context, 1)
	connections := make(chan *acp.AgentConn, 1)
	release := make(chan struct{})
	agent, err := acp.NewAgent(&acp.AgentConfig{
		NewSession: func(context.Context, *acp.AgentConn, *acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
			return &acp.NewSessionResponse{SessionID: "session"}, nil
		},
		Prompt: func(context.Context, *acp.AgentSession, *acp.PromptRequest) (*acp.PromptResponse, error) {
			return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		},
		AuthMethods: []acp.AuthMethod{&acp.AuthMethodAgent{ID: "token", Name: "Token"}},
		Authenticate: func(ctx context.Context, conn *acp.AgentConn, _ *acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
			contexts <- ctx
			connections <- conn
			<-release
			return &acp.AuthenticateResponse{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := connectPeers(t, client, agent)
	connectPeers(t, client, agent)
	answered := make(chan error, 1)
	go func() {
		_, err := conn.Authenticate(t.Context(), &acp.AuthenticateRequest{MethodID: "token"})
		answered <- err
	}()
	requestCtx, owner := <-contexts, <-connections
	params := &acp.CreateElicitationParams{Message: "?", Mode: &acp.ElicitationFormMode{RequestedSchema: formSchema()}}
	for candidate := range agent.Conns() {
		if candidate != owner {
			if _, err := candidate.CreateElicitation(requestCtx, params); err == nil {
				t.Error("another connection accepted the request's scope")
			}
		}
	}
	close(release)
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	<-requestCtx.Done()
	if _, err := owner.CreateElicitation(context.WithoutCancel(requestCtx), params); err == nil {
		t.Error("an answered request could still open a request-scoped elicitation")
	}
}

// The outbound half of the mode capability. A client that renders forms and not
// pages is not asked for a page, and the refusal happens before the write.
func TestAModeTheClientDidNotAdvertiseIsRefusedBeforeTheWire(t *testing.T) {
	var served int
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				served++
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var urlErr, completeErr error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, urlErr = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "sign in",
			Mode:    &acp.ElicitationURLMode{ElicitationID: "e-1", URL: "https://example.invalid/"},
		})
		completeErr = session.Conn().CompleteElicitation(ctx, &acp.CompleteElicitationNotification{
			ElicitationID: "e-1",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The mode is a parameter capability the client did not advertise, refused
	// locally before anything is sent.
	if !errors.Is(urlErr, acp.ErrNotAdvertised) {
		t.Fatalf("a url elicitation to a form-only client failed with %v, want ErrNotAdvertised", urlErr)
	}
	if !strings.Contains(urlErr.Error(), "clientCapabilities.elicitation.url") {
		t.Errorf("the refusal says %q, which does not name the capability", urlErr)
	}

	// The completion is gated on the url mode as a method, refused locally.
	if !errors.Is(completeErr, acp.ErrNotAdvertised) {
		t.Fatalf("elicitation/complete to a form-only client failed with %v, want ErrNotAdvertised", completeErr)
	}
	if served != 0 {
		t.Error("a refused elicitation still reached the client's handler")
	}
}

// The scope belongs to the operation, so a caller who sets one is told rather
// than having it replaced in silence.
func TestASuppliedScopeIsRefused(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var scoped, unknownMode, missingMode error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, scoped = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode: &acp.ElicitationFormMode{
				RequestedSchema: formSchema(),
				Value:           &acp.ElicitationFormModeSession{},
			},
		})
		_, unknownMode = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode:    &acp.CreateElicitationRequestOther{},
		})
		_, missingMode = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if scoped == nil || !strings.Contains(scoped.Error(), "scope") {
		t.Errorf("a mode carrying its own scope was answered %v, want a refusal naming the scope", scoped)
	}
	if unknownMode == nil || !strings.Contains(unknownMode.Error(), "ElicitationFormMode") {
		t.Errorf("the catch-all mode was answered %v, want a refusal naming the two sendable modes", unknownMode)
	}
	if missingMode == nil {
		t.Error("params with no mode were accepted")
	}
}

// A group that could not serve what it advertises is refused. Serving the URL
// mode without a completion handler is not one of those: the protocol makes
// sending a completion optional, so a client that never hears about one is a
// client that chose not to.
func TestElicitationConfigurationIsRefusedWhenItCannotBeServed(t *testing.T) {
	form := func(
		context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
		*acp.ElicitationFormMode,
	) (*acp.CreateElicitationResponse, error) {
		return &acp.CreateElicitationResponse{}, nil
	}
	url := func(
		context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
		*acp.ElicitationURLMode,
	) (*acp.CreateElicitationResponse, error) {
		return &acp.CreateElicitationResponse{}, nil
	}
	complete := func(context.Context, *acp.ClientConn, *acp.CompleteElicitationNotification) {}

	for name, handlers := range map[string]*acp.ElicitationHandlers{
		"neither mode":                  {},
		"a completion with no url mode": {Form: form, Complete: complete},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := acp.NewClient(&acp.ClientConfig{
				SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
				RequestPermission: denyingPermission,
				Elicitation:       handlers,
			})
			if err == nil {
				t.Fatal("the handler group was accepted and could not serve what it advertises")
			}
		})
	}

	t.Run("a url mode with no completion", func(t *testing.T) {
		_, err := acp.NewClient(&acp.ClientConfig{
			SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
			RequestPermission: denyingPermission,
			Elicitation:       &acp.ElicitationHandlers{URL: url},
		})
		if err != nil {
			t.Fatalf("a client that shows pages and does not want to hear they finished "+
				"was refused, and the protocol makes the completion optional: %v", err)
		}
	})
}

// A request-scoped elicitation has no session, so the tool call a session scope
// would carry is refused rather than dropped.
func TestARequestScopeRefusesAToolCall(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var withToolCall error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, withToolCall = session.Conn().CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message:    "paste your token",
			Mode:       &acp.ElicitationFormMode{RequestedSchema: formSchema()},
			ToolCallID: acp.OptValue(acp.ToolCallID("call-1")),
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, err := session.Prompt(context.Background(), &acp.PromptParams{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if withToolCall == nil || !strings.Contains(withToolCall.Error(), "ToolCallID") {
		t.Errorf("a tool call on a request scope was answered %v, want a refusal naming it", withToolCall)
	}
}

// Called outside a handler there is no request to be scoped to, and inventing one
// would send the client an elicitation tied to a call that is not happening.
func TestARequestScopeOutsideAHandlerIsRefused(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	agentConn := make(chan *acp.AgentConn, 1)
	agent := testAgent(t, func(
		_ context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		agentConn <- session.Conn()
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}

	// The prompt has been answered, so this context belongs to no request.
	conn := <-agentConn
	_, err = conn.CreateElicitation(context.Background(), &acp.CreateElicitationParams{
		Message: "paste your token",
		Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
	})
	if err == nil || !strings.Contains(err.Error(), "must be called from a handler") {
		t.Errorf("eliciting outside a handler was answered %v, want a refusal saying where it belongs", err)
	}
}

// The contracts a caller can break without a peer being involved at all. They are
// refused before anything is sent, and the message says which field.
func TestElicitationRefusesWhatACallerGetsWrong(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			URL: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationURLMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
			Complete: func(context.Context, *acp.ClientConn, *acp.CompleteElicitationNotification) {},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var noParams, noCompletionParams, scopedURL error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, noParams = session.CreateElicitation(ctx, nil)
		noCompletionParams = session.Conn().CompleteElicitation(ctx, nil)
		_, scopedURL = session.CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "sign in",
			Mode: &acp.ElicitationURLMode{
				ElicitationID: "e-1",
				URL:           "https://example.invalid/",
				Value:         &acp.ElicitationURLModeSession{},
			},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}

	for name, test := range map[string]struct {
		err   error
		wants string
	}{
		"an elicitation with no params":     {noParams, "Mode is required"},
		"a completion with no params":       {noCompletionParams, "is not outstanding"},
		"a url mode carrying its own scope": {scopedURL, "scope"},
	} {
		t.Run(name, func(t *testing.T) {
			if test.err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(test.err.Error(), test.wants) {
				t.Fatalf("refused with %q, which does not mention %q", test.err, test.wants)
			}
		})
	}
}

// A request-scoped elicitation is the schema's "tied to a specific JSON-RPC
// request outside of a session". A prompt has a session, so an elicitation from
// one belongs to that session — scoping it to the request would name a call the
// client already knows is part of a conversation.
func TestARequestScopeRefusesASessionScopedRequest(t *testing.T) {
	client, err := acp.NewClient(&acp.ClientConfig{
		SessionUpdate:     func(context.Context, *acp.ClientConn, *acp.SessionNotification) {},
		RequestPermission: denyingPermission,
		Elicitation: &acp.ElicitationHandlers{
			Form: func(
				context.Context, *acp.ClientConn, *acp.CreateElicitationRequest,
				*acp.ElicitationFormMode,
			) (*acp.CreateElicitationResponse, error) {
				return &acp.CreateElicitationResponse{Value: &acp.ElicitationAcceptAction{}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var fromPrompt error
	agent := testAgent(t, func(
		ctx context.Context,
		session *acp.AgentSession,
		_ *acp.PromptRequest,
	) (*acp.PromptResponse, error) {
		_, fromPrompt = session.Conn().CreateElicitation(ctx, &acp.CreateElicitationParams{
			Message: "which branch?",
			Mode:    &acp.ElicitationFormMode{RequestedSchema: formSchema()},
		})
		return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	})

	session := connectAndOpen(t, client, agent)
	if _, promptErr := session.Prompt(context.Background(), &acp.PromptParams{}); promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}
	if fromPrompt == nil || !strings.Contains(fromPrompt.Error(), "scoped to a session") {
		t.Errorf("a request scope taken from a prompt was answered %v, want a refusal naming "+
			"the session the request already has", fromPrompt)
	}
}
